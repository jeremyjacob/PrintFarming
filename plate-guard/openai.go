package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const plateAssessmentPrompt = `You are a conservative safety classifier for an automated FDM 3D-printer farm. Inspect the camera image and decide whether the entire printable build-plate area is clear enough to safely start another print.

Set plate_visible=false and is_empty=false if the build plate is obscured, cropped, too dark, blurry, or otherwise cannot be assessed reliably. Set is_empty=false if any printed part, loose object, substantial purge material, tool, hand, or other obstruction remains in the printable area. Normal surface texture, discoloration, and tiny harmless strands do not by themselves make a plate occupied. Never assume an ejection succeeded merely because this is a completion photo. Be conservative (is_empty=false) when uncertain.`

type plateAssessment struct {
	PlateVisible bool    `json:"plate_visible"`
	IsEmpty      bool    `json:"is_empty"`
	Confidence   float64 `json:"confidence"`
	Reason       string  `json:"reason"`
}

type openAIClient struct {
	apiKey      string
	baseURL     string
	model       string
	imageDetail string
	httpClient  *http.Client
}

func (c *openAIClient) assess(ctx context.Context, image []byte, mediaType string) (plateAssessment, error) {
	imageURL := "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(image)
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"plate_visible": map[string]any{
				"type":        "boolean",
				"description": "Whether the full printable build plate can be assessed reliably.",
			},
			"is_empty": map[string]any{
				"type":        "boolean",
				"description": "Whether the printable build plate is clear enough for another print.",
			},
			"confidence": map[string]any{
				"type":        "number",
				"minimum":     0,
				"maximum":     1,
				"description": "Confidence in the empty or occupied classification from 0 to 1.",
			},
			"reason": map[string]any{
				"type":        "string",
				"description": "A short factual reason for the classification.",
			},
		},
		"required":             []string{"plate_visible", "is_empty", "confidence", "reason"},
		"additionalProperties": false,
	}
	payload := map[string]any{
		"model": c.model,
		"store": false,
		"input": []any{
			map[string]any{
				"role":    "developer",
				"content": plateAssessmentPrompt,
			},
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": "Classify this build plate."},
					map[string]any{
						"type":      "input_image",
						"image_url": imageURL,
						"detail":    c.imageDetail,
					},
				},
			},
		},
		"text": map[string]any{
			"format": map[string]any{
				"type":   "json_schema",
				"name":   "plate_assessment",
				"strict": true,
				"schema": schema,
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return plateAssessment{}, fmt.Errorf("encode OpenAI request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/responses", bytes.NewReader(body))
	if err != nil {
		return plateAssessment{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return plateAssessment{}, fmt.Errorf("call OpenAI Responses API: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return plateAssessment{}, responseError("OpenAI Responses API", resp)
	}

	var response struct {
		Status string `json:"status"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Refusal string `json:"refusal"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&response); err != nil {
		return plateAssessment{}, fmt.Errorf("decode OpenAI response: %w", err)
	}
	if response.Error != nil {
		return plateAssessment{}, fmt.Errorf("OpenAI response error: %s", response.Error.Message)
	}
	if response.Status != "completed" {
		return plateAssessment{}, fmt.Errorf("OpenAI response status is %q", response.Status)
	}

	var outputText string
	for _, output := range response.Output {
		if output.Type != "message" {
			continue
		}
		for _, content := range output.Content {
			switch content.Type {
			case "output_text":
				outputText = content.Text
			case "refusal":
				return plateAssessment{}, fmt.Errorf("OpenAI refused the image assessment: %s", content.Refusal)
			}
		}
	}
	if strings.TrimSpace(outputText) == "" {
		return plateAssessment{}, fmt.Errorf("OpenAI response did not contain output text")
	}

	var assessment plateAssessment
	if err := json.Unmarshal([]byte(outputText), &assessment); err != nil {
		return plateAssessment{}, fmt.Errorf("decode OpenAI plate assessment: %w", err)
	}
	if assessment.Confidence < 0 || assessment.Confidence > 1 {
		return plateAssessment{}, fmt.Errorf("OpenAI returned confidence outside 0..1")
	}
	if strings.TrimSpace(assessment.Reason) == "" {
		return plateAssessment{}, fmt.Errorf("OpenAI returned an empty assessment reason")
	}
	return assessment, nil
}
