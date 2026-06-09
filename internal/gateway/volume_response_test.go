package gateway

import "testing"

func TestVolumeResponsesSynthesizeCompatTokenFromVolumeID(t *testing.T) {
	volume := RuntimeVolume{
		VolumeID: "vol-1",
		Name:     "data",
	}

	apiPayload := apiVolumeAndToken(volume)
	if apiPayload.Token != "compat-volume-token-vol-1" {
		t.Fatalf("expected synthesized api token, got %#v", apiPayload)
	}

	appPayload := (&App{}).volumeResponse(volume)
	if appPayload.Token != "compat-volume-token-vol-1" {
		t.Fatalf("expected synthesized app token, got %#v", appPayload)
	}
}

func TestVolumeResponsesReturnEmptyCompatTokenWithoutVolumeID(t *testing.T) {
	volume := RuntimeVolume{
		VolumeID: " ",
		Name:     "data",
	}

	if got := apiVolumeAndToken(volume).Token; got != "" {
		t.Fatalf("expected empty api token, got %q", got)
	}
	if got := (&App{}).volumeResponse(volume).Token; got != "" {
		t.Fatalf("expected empty app token, got %q", got)
	}
}
