package media

import "testing"

// goldenTimelineFilter 来自 Java 仓库金样 src/test/resources/parity/tools.ai-video-translate/qwen_tts_subtitle_erase.json
// （steps[53].detail.argv[51]）。两边的音轨拼接滤镜必须逐字一致；金样更新时这里要同步更新。
const goldenTimelineFilter = `[0:a]adelay=300:all=1[a0];[1:a]adelay=2440:all=1[a1];[2:a]adelay=4580:all=1[a2];[3:a]adelay=6720:all=1[a3];[4:a]adelay=8860:all=1[a4];[5:a]adelay=11000:all=1[a5];[6:a]adelay=13140:all=1[a6];[7:a]adelay=15280:all=1[a7];[8:a]adelay=17420:all=1[a8];[9:a]adelay=19560:all=1[a9];[10:a]adelay=21820:all=1[a10];[11:a]adelay=24080:all=1[a11];[12:a]adelay=26340:all=1[a12];[13:a]adelay=28600:all=1[a13];[14:a]adelay=30860:all=1[a14];[15:a]adelay=33120:all=1[a15];[16:a]adelay=35380:all=1[a16];[17:a]adelay=37640:all=1[a17];[18:a]adelay=39900:all=1[a18];[19:a]adelay=42160:all=1[a19];[20:a]adelay=44420:all=1[a20];[21:a]adelay=46560:all=1[a21];[22:a]adelay=48700:all=1[a22];[a0][a1][a2][a3][a4][a5][a6][a7][a8][a9][a10][a11][a12][a13][a14][a15][a16][a17][a18][a19][a20][a21][a22]amix=inputs=23:normalize=0,apad,atrim=duration=56.04,asetpts=N/SR/TB[aout]`

// goldenStarts / goldenTotal 是金样里的开始时间与总时长（由 adelay 与 atrim 还原）。
var goldenStarts = []float64{0.3, 2.44, 4.58, 6.72, 8.86, 11, 13.14, 15.28, 17.42, 19.56, 21.82, 24.08, 26.34, 28.6, 30.86, 33.12, 35.38, 37.64, 39.9, 42.16, 44.42, 46.56, 48.7}

const goldenTotal = 56.04

func TestTimelineFilterMatchesGoldenFromJavaRepo(t *testing.T) {
	got, err := TimelineFilter(goldenStarts, goldenTotal)
	if err != nil {
		t.Fatal(err)
	}
	if got != goldenTimelineFilter {
		t.Fatalf("音轨拼接滤镜与金样不一致\n got: %s\nwant: %s", got, goldenTimelineFilter)
	}
}
