package media

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

// TokenHeader 调用方鉴权头。
const TokenHeader = "X-Media-Token"

// apiPrefix 契约路径前缀（media.api）。/healthz 不带前缀。
const apiPrefix = "/v1/mediasvc"

// Server 媒体服务的 HTTP 入口，契约见 app/media/api/media.api 与 docs/api/media-service.md。
type Server struct {
	paths *pathCache
	// objCache 按 rid 的对象字节缓存（object_cache.go）；nil 表示关闭
	objCache *objectCache
	svc      *Service
	objects  ObjectStore
	idem     idemStore
	id       *identity
	// opCeiling 单次请求超时的上限（来自 OpTimeoutSeconds）；deadline_ms 超过它时截到它
	opCeiling time.Duration
	token     string
	opsToken  string
	mux       *http.ServeMux
	// owners 为新资源选择所属实例（按幂等键 rendezvous 哈希，候选是注册中心里存活的实例）
	owners *ownerPicker
	// closers 关闭时释放的资源（注册中心客户端）
	closers []func()
	// now 时钟（测试注入）；软删除的宽限期与清理都以它为准
	now func() time.Time
	// grace 软删除宽限期；sweepEvery 清理间隔
	grace      time.Duration
	sweepEvery time.Duration
}

// Close 释放服务持有的注册中心客户端。
func (s *Server) Close() {
	for _, c := range s.closers {
		c()
	}
}

// NewServer token 为空直接报错：这是能让人把任意地址拉下来处理的服务，不允许无鉴权启动。
func NewServer(cfg Config) (*Server, error) {
	if cfg.Token == "" {
		return nil, errors.New("media: 必须配置 Token，拒绝无鉴权启动")
	}
	id, err := newIdentity(cfg.InstanceID, cfg.IdentitySecret)
	if err != nil {
		return nil, err
	}
	var objects ObjectStore
	if cfg.OSS.Enabled {
		store, err := NewOSSStore(cfg.OSS)
		if err != nil {
			return nil, err
		}
		objects = store
	} else {
		if cfg.ObjectDir == "" {
			return nil, errors.New("media: 未启用 OSS 时必须配置 ObjectDir（本地对象目录）")
		}
		store, err := NewLocalStore(cfg.ObjectDir)
		if err != nil {
			return nil, err
		}
		objects = store
	}
	idemTTL := time.Duration(cfg.IdempotencyMinutes) * time.Minute
	idem, closeIdem, err := newIdemStoreFromConfig(cfg.Idempotency, idemTTL, defaultIdemLease)
	if err != nil {
		return nil, err
	}
	if cfg.DeleteGraceMinutes < 0 {
		return nil, errors.New("media: DeleteGraceMinutes 不能为负")
	}
	sweepEvery := time.Duration(cfg.SweepIntervalMinutes) * time.Minute
	if sweepEvery <= 0 {
		sweepEvery = time.Hour
	}
	s := &Server{svc: NewService(cfg), objects: objects, id: id, idem: idem, opCeiling: time.Duration(cfg.OpTimeoutSeconds) * time.Second, token: cfg.Token, opsToken: cfg.OpsToken, mux: http.NewServeMux(), paths: newPathCache(),
		now: time.Now, grace: time.Duration(cfg.DeleteGraceMinutes) * time.Minute, sweepEvery: sweepEvery}
	s.closers = append(s.closers, closeIdem)
	if cfg.ObjectCacheMaxBytes > 0 && cfg.ObjectCacheMinutes > 0 {
		s.objCache = newObjectCache(cfg.TempDir, time.Duration(cfg.ObjectCacheMinutes)*time.Minute,
			effectiveCacheCapShare(cfg.ObjectCacheMaxBytes, cfg.TempDir, objectCacheDiskShare))
		stop := make(chan struct{})
		go func() {
			t := time.NewTicker(30 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case now := <-t.C:
					s.objCache.sweep(now)
				}
			}
		}()
		var once sync.Once
		s.closers = append(s.closers, func() { once.Do(func() { close(stop) }) })
	}
	s.owners = newOwnerPicker(cfg, id.instanceID, &s.closers)
	s.mux.HandleFunc("GET /healthz", s.healthz)
	// /healthz 只是存活；/readyz 查对象存储可达 + 幂等 Redis，失败 503（供负载均衡 / k8s readinessProbe）
	s.mux.HandleFunc("GET /readyz", s.newReady().Handler())
	s.mux.HandleFunc("GET "+apiPrefix+"/capabilities", s.auth(s.capabilities))
	s.mux.HandleFunc("POST "+apiPrefix+"/ingest/upload", s.auth(s.uploadBytes))
	s.mux.HandleFunc("POST "+apiPrefix+"/ingest", s.auth(s.withDeadline(s.stageTimed(s.idempotent("ingest", s.ingest)))))
	s.mux.HandleFunc("POST "+apiPrefix+"/probe", s.auth(s.withDeadline(s.probe)))
	s.mux.HandleFunc("POST "+apiPrefix+"/video/inspect", s.auth(s.withDeadline(s.videoInspect)))
	s.mux.HandleFunc("POST "+apiPrefix+"/video/motion-profile", s.auth(s.withDeadline(s.videoMotionProfile)))
	s.mux.HandleFunc("POST "+apiPrefix+"/frames", s.auth(s.withDeadline(s.frames)))
	s.mux.HandleFunc("POST "+apiPrefix+"/cover", s.auth(s.withDeadline(s.idempotent("cover", s.cover))))
	s.mux.HandleFunc("POST "+apiPrefix+"/watermark", s.auth(s.withDeadline(s.idempotent("watermark", s.watermark))))
	s.mux.HandleFunc("POST "+apiPrefix+"/video/trim", s.auth(s.withDeadline(s.idempotent("video/trim", s.trim))))
	s.mux.HandleFunc("POST "+apiPrefix+"/video/conform", s.auth(s.withDeadline(s.idempotent("video/conform", s.conform))))
	s.mux.HandleFunc("POST "+apiPrefix+"/resources/upload-target", s.auth(s.withDeadline(s.uploadTarget)))
	s.mux.HandleFunc("POST "+apiPrefix+"/resources/register", s.auth(s.withDeadline(s.register)))
	s.mux.HandleFunc("POST "+apiPrefix+"/resources/urls", s.auth(s.withDeadline(s.resourceURL)))
	if s.opsToken != "" {
		s.mux.HandleFunc("POST "+apiPrefix+"/ops/locate", s.opsAuth(s.locate))
		// Prometheus 指标（通道排队 / 占用、对象写入耗时），与 locate 同用运维令牌
		s.mux.HandleFunc("GET "+apiPrefix+"/metrics", s.opsAuth(metricsHandler()))
	}
	s.mux.HandleFunc("POST "+apiPrefix+"/resources/copy", s.auth(s.withDeadline(s.stageTimed(s.idempotent("resources/copy", s.copyResource)))))
	s.mux.HandleFunc("POST "+apiPrefix+"/image/conform", s.auth(s.withDeadline(s.idempotent("image/conform", s.imageConform))))
	s.mux.HandleFunc("POST "+apiPrefix+"/image/resize", s.auth(s.withDeadline(s.idempotent("image/resize", s.imageResize))))
	s.mux.HandleFunc("POST "+apiPrefix+"/image/watermark", s.auth(s.withDeadline(s.idempotent("image/watermark", s.imageWatermark))))
	s.mux.HandleFunc("POST "+apiPrefix+"/image/grid", s.auth(s.withDeadline(s.idempotent("image/grid", s.imageGrid))))
	s.mux.HandleFunc("POST "+apiPrefix+"/image/crop", s.auth(s.withDeadline(s.idempotent("image/crop", s.imageCrop))))
	s.mux.HandleFunc("POST "+apiPrefix+"/image/mark", s.auth(s.withDeadline(s.idempotent("image/mark", s.imageMark))))
	s.mux.HandleFunc("POST "+apiPrefix+"/compose", s.auth(s.withDeadline(s.idempotent("compose", s.compose))))
	s.mux.HandleFunc("POST "+apiPrefix+"/audio/extract", s.auth(s.withDeadline(s.idempotent("audio/extract", s.extractAudio))))
	s.mux.HandleFunc("POST "+apiPrefix+"/audio/timeline", s.auth(s.withDeadline(s.idempotent("audio/timeline", s.audioTimeline))))
	s.mux.HandleFunc("POST "+apiPrefix+"/video/replace-audio", s.auth(s.withDeadline(s.idempotent("video/replace-audio", s.replaceAudio))))
	s.mux.HandleFunc("POST "+apiPrefix+"/video/burn-subtitles", s.auth(s.withDeadline(s.idempotent("video/burn-subtitles", s.burnSubtitles))))
	s.mux.HandleFunc("POST "+apiPrefix+"/video/adjust-duration", s.auth(s.withDeadline(s.idempotent("video/adjust-duration", s.adjustDuration))))
	s.mux.HandleFunc("POST "+apiPrefix+"/video/cut", s.auth(s.withDeadline(s.idempotent("video/cut", s.videoCut))))
	s.mux.HandleFunc("POST "+apiPrefix+"/video/concat", s.auth(s.withDeadline(s.idempotent("video/concat", s.videoConcat))))
	s.mux.HandleFunc("POST "+apiPrefix+"/video/mux-audio", s.auth(s.withDeadline(s.idempotent("video/mux-audio", s.videoMuxAudio))))
	s.mux.HandleFunc("POST "+apiPrefix+"/video/reframe", s.auth(s.withDeadline(s.idempotent("video/reframe", s.videoReframe))))
	s.mux.HandleFunc("POST "+apiPrefix+"/video/overlay-text", s.auth(s.withDeadline(s.idempotent("video/overlay-text", s.videoOverlayText))))
	s.mux.HandleFunc("POST "+apiPrefix+"/image/text", s.auth(s.withDeadline(s.idempotent("image/text", s.imageText))))
	s.mux.HandleFunc("POST "+apiPrefix+"/resources/restore", s.auth(s.withDeadline(s.restore)))
	s.mux.HandleFunc("POST "+apiPrefix+"/delete", s.auth(s.withDeadline(s.idempotent("delete", s.delete))))
	s.mux.HandleFunc("POST "+apiPrefix+"/resources/retention/delete", s.auth(s.withDeadline(s.idempotent("resources/retention/delete", s.retentionDelete))))
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { withTrace(s.mux).ServeHTTP(w, r) }

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok")
}

// auth 常量时间比较 token，避免靠响应时间猜出来。两边先过 SHA-256 再比，定长比较，不泄露 token 长度。
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	want := sha256.Sum256([]byte(s.token))
	return func(w http.ResponseWriter, r *http.Request) {
		got := sha256.Sum256([]byte(bearerOrToken(r)))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			writeAPIError(w, newAPIError(codeUnauthorized, "auth.token"))
			return
		}
		next(w, r)
	}
}
