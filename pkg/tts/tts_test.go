package tts

import (
	"encoding/binary"
	"testing"
	"time"
)

// wavBytes 构造百炼风格的 wav：头里长度字段是 0x7FFFFFFF 占位。
func wavBytes(samples int, placeholder bool) []byte {
	const sr, ch, bits = 24000, 1, 16
	data := samples * ch * bits / 8
	b := make([]byte, 44+data)
	copy(b[0:], "RIFF")
	copy(b[8:], "WAVE")
	copy(b[12:], "fmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], ch)
	binary.LittleEndian.PutUint32(b[24:], sr)
	binary.LittleEndian.PutUint32(b[28:], sr*ch*bits/8)
	binary.LittleEndian.PutUint16(b[32:], ch*bits/8)
	binary.LittleEndian.PutUint16(b[34:], bits)
	copy(b[36:], "data")
	size := uint32(data)
	if placeholder {
		size = 0x7FFFFFFF
		binary.LittleEndian.PutUint32(b[4:], 0x7FFFFFFF)
	}
	binary.LittleEndian.PutUint32(b[40:], size)
	return b
}

func TestWavDuration(t *testing.T) {
	for _, placeholder := range []bool{false, true} {
		d, err := WavDuration(wavBytes(24000*5+12000, placeholder))
		if err != nil {
			t.Fatal(err)
		}
		if d != 5500*time.Millisecond {
			t.Fatalf("placeholder=%v got %v", placeholder, d)
		}
	}
}

func TestWavDurationInvalid(t *testing.T) {
	if _, err := WavDuration([]byte("not a wav file at all")); err == nil {
		t.Fatal("want error")
	}
}
