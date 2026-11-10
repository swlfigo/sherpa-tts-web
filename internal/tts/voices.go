package tts

// Voice is a speaker of the Kokoro v1.0 multi-lingual model. Only the
// Chinese and English speakers are listed: the model ships lexicons for
// those two languages.
type Voice struct {
	Name    string `json:"name"` // as in the model's metadata, e.g. bf_emma
	SID     int    `json:"-"`
	Group   string `json:"group"`   // 中文女声, 美式男声...
	Display string `json:"display"` // 晓晓, Emma...
	Lang    string `json:"lang"`    // zh or en
}

// DefaultVoice is used when none is chosen.
const DefaultVoice = "af_heart"

// Voices lists the speakers, grouped, the recommended first in each group.
var Voices = func() []Voice {
	type v struct {
		name, display string
		sid           int
	}
	groups := []struct {
		group, lang string
		list        []v
	}{
		{"中文女声", "zh", []v{{"zf_xiaoxiao", "晓晓", 47}, {"zf_xiaoyi", "晓伊", 48}, {"zf_xiaobei", "晓北", 45}, {"zf_xiaoni", "晓妮", 46}}},
		{"中文男声", "zh", []v{{"zm_yunxi", "云希", 50}, {"zm_yunjian", "云健", 49}, {"zm_yunxia", "云夏", 51}, {"zm_yunyang", "云扬", 52}}},
		{"美式女声", "en", []v{{"af_heart", "Heart", 3}, {"af_bella", "Bella", 2}, {"af_nicole", "Nicole", 6}, {"af_sarah", "Sarah", 9}, {"af_nova", "Nova", 7}, {"af_sky", "Sky", 10}, {"af_alloy", "Alloy", 0}, {"af_aoede", "Aoede", 1}, {"af_jessica", "Jessica", 4}, {"af_kore", "Kore", 5}, {"af_river", "River", 8}}},
		{"美式男声", "en", []v{{"am_michael", "Michael", 16}, {"am_fenrir", "Fenrir", 14}, {"am_puck", "Puck", 18}, {"am_adam", "Adam", 11}, {"am_echo", "Echo", 12}, {"am_eric", "Eric", 13}, {"am_liam", "Liam", 15}, {"am_onyx", "Onyx", 17}, {"am_santa", "Santa", 19}}},
		{"英式女声", "en", []v{{"bf_emma", "Emma", 21}, {"bf_isabella", "Isabella", 22}, {"bf_alice", "Alice", 20}, {"bf_lily", "Lily", 23}}},
		{"英式男声", "en", []v{{"bm_george", "George", 26}, {"bm_fable", "Fable", 25}, {"bm_daniel", "Daniel", 24}, {"bm_lewis", "Lewis", 27}}},
	}
	var out []Voice
	for _, g := range groups {
		for _, x := range g.list {
			out = append(out, Voice{Name: x.name, SID: x.sid, Group: g.group, Display: x.display, Lang: g.lang})
		}
	}
	return out
}()

// VoiceByName finds a voice.
func VoiceByName(name string) (Voice, bool) {
	for _, v := range Voices {
		if v.Name == name {
			return v, true
		}
	}
	return Voice{}, false
}

// PreviewText is the sentence a voice says when previewed.
func PreviewText(v Voice) string {
	if v.Lang == "zh" {
		return "你好，这是我的声音，欢迎使用文字转语音。"
	}
	return "Hello, this is my voice. Thanks for trying text to speech."
}
