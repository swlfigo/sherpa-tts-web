package tts

import (
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

func TestNormalize(t *testing.T) {
	in := "Opening Hours\n" +
		"The library is open from nine in the morning until six in the evening on\n" +
		"weekdays, and until noon on Saturdays."
	got := Normalize(in)
	if !strings.HasPrefix(got, "Opening Hours. The library") {
		t.Errorf("heading not ended: %q", got)
	}
	if strings.Contains(got, "on. weekdays") {
		t.Errorf("wrapped line split: %q", got)
	}
	if got := Normalize("  a \n\n  b  "); got != "a b" {
		t.Errorf("Normalize = %q", got)
	}
}

func TestFileName(t *testing.T) {
	at := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	if got := FileName("你好，世界! Hello", "zf_xiaoxiao", at); got != "你好_世界_Hello_zf_xiaoxiao_20261110-120000.wav" {
		t.Errorf("FileName = %q", got)
	}
	if got := FileName("……", "af_heart", at); got != "speech_af_heart_20261110-120000.wav" {
		t.Errorf("FileName = %q", got)
	}
}

func TestEncodeWAV(t *testing.T) {
	b := EncodeWAV(&Audio{Samples: []float32{0, 1, -1, 2}, SampleRate: 24000})
	if len(b) != 44+8 || string(b[:4]) != "RIFF" || string(b[36:40]) != "data" {
		t.Fatalf("bad header: % x", b[:44])
	}
	if sr := binary.LittleEndian.Uint32(b[24:]); sr != 24000 {
		t.Errorf("sample rate %d", sr)
	}
	if v := int16(binary.LittleEndian.Uint16(b[50:])); v != 32767 {
		t.Errorf("clipped sample %d", v)
	}
}

func TestVoices(t *testing.T) {
	seen := map[int]bool{}
	for _, v := range Voices {
		if v.SID < 0 || v.SID > 53 || seen[v.SID] {
			t.Errorf("bad or repeated sid %d (%s)", v.SID, v.Name)
		}
		seen[v.SID] = true
		if v.Lang != "zh" && v.Lang != "en" {
			t.Errorf("%s: lang %q", v.Name, v.Lang)
		}
	}
	if _, ok := VoiceByName(DefaultVoice); !ok {
		t.Error("default voice missing")
	}
}
