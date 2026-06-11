package gateway

import "testing"

func TestAPIVolumeResponsesSynthesizeCompatTokenFromVolumeID(t *testing.T) {
	volume := RuntimeVolume{
		VolumeID: "vol-1",
		Name:     "data",
	}

	apiPayload := apiVolumeAndToken(volume)
	if apiPayload.Token != "compat-volume-token-vol-1" {
		t.Fatalf("expected synthesized api token, got %#v", apiPayload)
	}
}

func TestAPIVolumeResponsesReturnEmptyCompatTokenWithoutVolumeID(t *testing.T) {
	volume := RuntimeVolume{
		VolumeID: " ",
		Name:     "data",
	}

	if got := apiVolumeAndToken(volume).Token; got != "" {
		t.Fatalf("expected empty api token, got %q", got)
	}
}
