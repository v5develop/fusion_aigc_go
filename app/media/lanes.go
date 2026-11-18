package media

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// metricsHandler 暴露 Prometheus 文本格式指标。
func metricsHandler() http.HandlerFunc {
	h := promhttp.HandlerFor(metricsRegistry(), promhttp.HandlerOpts{})
	return h.ServeHTTP
}

// laneKind 名额通道。三条通道各有独立信号量，互不排队：
//
//   - laneVideo：视频重算力（ffmpeg 转码 / 合成 / 重构图 / 抽帧 / 抽音轨 / 视频解码检查），一个任务可跑几十秒到几分钟，吃满 CPU；
//   - laneLight：轻算力（ffprobe 元数据、图片的 ffmpeg 解码 / 缩放 / 裁切 / 叠字 / 打标、像素扫描），毫秒到几秒；
//   - laneIO：下载、请求体读取、对象存储读写与服务端复制，瓶颈是网络而不是 CPU。
//
// 拆开的原因：网关把每一张生成图 / 每一段生成视频都经本服务转存，若图片的 probe / 校验 / 落桶与视频转码共用同一组名额，
// 一批视频合成就会让图片排队到 503。
type laneKind int

const (
	laneVideo laneKind = iota
	laneLight
	laneIO
	laneCount
)

// 通道名（指标标签 / 日志）。
var laneNames = [laneCount]string{"video_compute", "light_compute", "io"}

// 排队阶段名（响应 timings / 日志）。视频算力沿用历史的 queue_compute，输入名额沿用 queue_io。
var laneStages = [laneCount]string{stageQueueCompute, stageQueueLight, stageQueueIO}

const stageQueueLight = "queue_light"

// 各通道默认大小：
//
//	VideoCompute = max(4, 有效核数 / FFmpegThreads) —— 每个视频 ffmpeg 约占 FFmpegThreads 个核；不低于旧默认 MaxConcurrent=4
//	LightCompute = 有效核数 * 2                     —— 图片 / ffprobe 很短，且图片 ffmpeg 固定单线程（LightThreads，默认 1）
//	IO           = 32                               —— 网络等待型，只防写满磁盘与连接数
//
// 有效核数 = min(GOMAXPROCS, cgroup CPU 配额)，见 cpu.go（容器限核时不会按宿主机核数推导）。
const defaultIOLane = 32

// laneSizes 由配置推导三条通道的大小。优先级：显式的 VideoCompute / LightCompute / IO，
// 其次 MaxConcurrent（向后兼容，只作为 VideoCompute 的别名），最后按 CPU 核数推导。
// ApplyEnvOverrides 把并发名额相关的环境变量覆盖到配置上（留空或未设置忽略；非正整数返回错误）。
// 不放进 yaml 的 ${VAR}：go-zero 里整数字段遇到空字符串会直接加载失败，新旧 compose 混用时会让服务起不来。
func ApplyEnvOverrides(c *Config, getenv func(string) string) error {
	set := func(name string, dst *int) error {
		v := strings.TrimSpace(getenv(name))
		if v == "" {
			return nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return fmt.Errorf("media: %s=%q 必须为正整数（留空表示按默认）", name, v)
		}
		*dst = n
		return nil
	}
	for _, it := range []struct {
		name string
		dst  *int
	}{
		{"MEDIA_VIDEO_COMPUTE", &c.VideoCompute},
		{"MEDIA_LIGHT_COMPUTE", &c.LightCompute},
		{"MEDIA_IO_SLOTS", &c.IO},
		{"MEDIA_LIGHT_THREADS", &c.LightThreads},
		{"MEDIA_QUEUE_WAIT_SECONDS", &c.QueueWaitSeconds},
		{"MEDIA_OSS_MAX_IDLE_CONNS_PER_HOST", &c.OSS.MaxIdleConnsPerHost},
	} {
		if err := set(it.name, it.dst); err != nil {
			return err
		}
	}
	return nil
}

func laneSizes(cfg Config, numCPU int) [laneCount]int {
	if numCPU < 1 {
		numCPU = 1
	}
	threads := cfg.FFmpegThreads
	if threads <= 0 {
		threads = 2
	}
	var sz [laneCount]int
	switch {
	case cfg.VideoCompute > 0:
		sz[laneVideo] = cfg.VideoCompute
	case cfg.MaxConcurrent > 0:
		sz[laneVideo] = cfg.MaxConcurrent
	default:
		sz[laneVideo] = max(4, numCPU/threads)
	}
	if cfg.LightCompute > 0 {
		sz[laneLight] = cfg.LightCompute
	} else {
		sz[laneLight] = numCPU * 2
	}
	switch {
	case cfg.IO > 0:
		sz[laneIO] = cfg.IO
	default:
		sz[laneIO] = defaultIOLane
	}
	return sz
}

func newLanes(cfg Config) [laneCount]chan struct{} {
	sz := laneSizes(cfg, effectiveCPUs())
	var out [laneCount]chan struct{}
	for i := range out {
		out[i] = make(chan struct{}, sz[i])
		laneCapacity.WithLabelValues(laneNames[i]).Set(float64(sz[i]))
	}
	return out
}

// ---- 指标（Prometheus，默认注册表；GET /v1/mediasvc/metrics 暴露，运维令牌鉴权） ----

var (
	laneInUse = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "media_lane_inuse", Help: "各通道当前占用的名额数",
	}, []string{"lane"})
	laneCapacity = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "media_lane_capacity", Help: "各通道的名额总数",
	}, []string{"lane"})
	laneWait = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "media_lane_wait_seconds", Help: "排队等待名额的时长（秒）；result=ok 拿到名额，timeout 排队超时，canceled 请求先结束",
		Buckets: []float64{0.001, 0.01, 0.05, 0.1, 0.5, 1, 2, 5, 10, 30, 60, 120},
	}, []string{"lane", "result"})
	putDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "media_put_duration_seconds", Help: "对象写入（Put / 服务端复制）耗时（秒），不含排队名额",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 40, 80, 160},
	}, []string{"store", "result"})
)

var registerMetricsOnce sync.Once

// metricsRegistry 返回含本服务指标的注册表（只注册一次；测试里多次 NewService 也安全）。
func metricsRegistry() *prometheus.Registry {
	registerMetricsOnce.Do(func() {
		metricsReg = prometheus.NewRegistry()
		metricsReg.MustRegister(laneInUse, laneCapacity, laneWait, putDuration)
	})
	return metricsReg
}

var metricsReg *prometheus.Registry

func observeWait(l laneKind, start time.Time, result string) {
	laneWait.WithLabelValues(laneNames[l], result).Observe(time.Since(start).Seconds())
}

func observePut(store string, start time.Time, err error) {
	res := "ok"
	if err != nil {
		res = "error"
	}
	putDuration.WithLabelValues(store, res).Observe(time.Since(start).Seconds())
}
