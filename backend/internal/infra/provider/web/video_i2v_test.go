package web

import "testing"

func TestImageToVideoPayloadMatchesGrokImagine(t *testing.T) {
	payload := imageToVideoPayload("turn slowly", "asset-1", "16:9", "720p", 6)
	if payload["message"] != "turn slowly --mode=custom" || payload["modelName"] != "imagine-video-gen" {
		t.Fatalf("payload header = %#v", payload)
	}
	input := payload["mediaGenInput"].(map[string]any)["imageToVideo"].(map[string]any)
	assets := input["inputAssets"].([]string)
	if len(assets) != 1 || assets[0] != "asset-1" || input["mode"] != "custom" || input["duration"] != 6 || input["resolutionName"] != "720p" || input["aspectRatio"] != "16:9" {
		t.Fatalf("imageToVideo = %#v", input)
	}
	empty := imageToVideoPayload("", "asset-1", "", "720p", 6)
	if empty["message"] != "--mode=custom" {
		t.Fatalf("empty prompt message = %#v", empty["message"])
	}
	if _, ok := empty["mediaGenInput"].(map[string]any)["imageToVideo"].(map[string]any)["prompt"]; ok {
		t.Fatalf("empty prompt must be omitted")
	}
}

func TestReferenceToVideoPayload(t *testing.T) {
	payload := referenceToVideoPayload("a dog", []string{"a", "b"}, "", "720p", 6)
	input := payload["mediaGenInput"].(map[string]any)["referenceToVideo"].(map[string]any)
	if len(input["inputAssets"].([]string)) != 2 || input["prompt"] != "a dog" {
		t.Fatalf("referenceToVideo = %#v", input)
	}
}
