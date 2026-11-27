package androidtv

import "testing"

func TestParseResFollowsAdaptiveRamp(t *testing.T) {
	out := `         1795754086.803     0     0 D         : [120690.210252@3]  hevc video changed to 3840 x 2160 60 fps clk->800MHZ
         1795754086.914     0     0 I [120690.262909@0]  [475]: Parse from ucode, visible(1024 x 576), coded(1024 x 576), scan:P
         1795754090.670     0     0 I [120694.053143@0]  [477]: Parse from ucode, visible(1920 x 1080), coded(1920 x 1088), scan:P
         1795754094.789     0     0 I [120698.196075@0]  [477]: Parse from ucode, visible(3840 x 2160), coded(3840 x 2176), scan:P`
	evs, last := parseRes(out, 0)
	if len(evs) != 3 || evs[2].width != 3840 || evs[2].height != 2160 || evs[0].codec != "HEVC" {
		t.Fatalf("got %+v", evs)
	}
	if again, _ := parseRes(out, last); len(again) != 0 {
		t.Fatalf("re-read gave %+v", again)
	}
}
