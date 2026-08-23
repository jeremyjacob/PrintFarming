package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAIAssessUsesVisionAndStructuredOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer openai-key" {
			http.Error(w, "missing API key", http.StatusUnauthorized)
			return
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(payload)
		requestJSON := string(encoded)
		if !strings.Contains(requestJSON, `data:image/jpeg;base64,`) {
			t.Fatalf("request did not contain a JPEG data URL: %s", requestJSON)
		}
		if !strings.Contains(requestJSON, `json_schema`) {
			t.Fatalf("request did not contain a JSON schema: %s", requestJSON)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "completed",
			"output": []any{
				map[string]any{
					"type": "message",
					"content": []any{
						map[string]any{
							"type": "output_text",
							"text": `{"plate_visible":true,"is_empty":true,"confidence":0.99,"reason":"The build area is clear."}`,
						},
					},
				},
			},
		})
	}))
	defer server.Close()

	client := &openAIClient{
		apiKey:      "openai-key",
		baseURL:     server.URL,
		model:       "test-model",
		imageDetail: "high",
		httpClient:  server.Client(),
	}
	assessment, err := client.assess(context.Background(), []byte{0xff, 0xd8, 0xff, 0xdb}, "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if !assessment.PlateVisible || !assessment.IsEmpty || assessment.Confidence != 0.99 {
		t.Fatalf("unexpected assessment: %+v", assessment)
	}
}

func TestOpenAIAssessRequiresCompletedStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"output": []any{
				map[string]any{
					"type": "message",
					"content": []any{
						map[string]any{
							"type": "output_text",
							"text": `{"plate_visible":true,"is_empty":true,"confidence":0.99,"reason":"clear"}`,
						},
					},
				},
			},
		})
	}))
	defer server.Close()

	client := &openAIClient{
		apiKey:      "openai-key",
		baseURL:     server.URL,
		model:       "gpt-5.6-terra",
		imageDetail: "high",
		httpClient:  server.Client(),
	}
	if _, err := client.assess(context.Background(), []byte{0xff, 0xd8, 0xff, 0xdb}, "image/jpeg"); err == nil {
		t.Fatal("expected a response without completed status to fail")
	}
}

func TestOpenAIAssessFirstLayerUsesSpecializedConservativePrompt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(payload)
		requestJSON := string(encoded)
		for _, required := range []string{
			`"name":"first_layer_assessment"`,
			`"is_defective"`,
			`False alarms are costly`,
			`do not know the intended model`,
		} {
			if !strings.Contains(requestJSON, required) {
				t.Fatalf("first-layer request missing %q: %s", required, requestJSON)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "completed",
			"output": []any{
				map[string]any{
					"type": "message",
					"content": []any{
						map[string]any{
							"type": "output_text",
							"text": `{"first_layer_visible":true,"is_defective":true,"confidence":0.995,"reason":"Detached extrusion is visibly tangled."}`,
						},
					},
				},
			},
		})
	}))
	defer server.Close()

	client := &openAIClient{
		apiKey:      "openai-key",
		baseURL:     server.URL,
		model:       "test-model",
		imageDetail: "high",
		httpClient:  server.Client(),
	}
	assessment, err := client.assessFirstLayer(context.Background(), []byte{0xff, 0xd8, 0xff, 0xdb}, "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if !assessment.FirstLayerVisible || !assessment.IsDefective || assessment.Confidence != 0.995 {
		t.Fatalf("unexpected first-layer assessment: %+v", assessment)
	}
}
