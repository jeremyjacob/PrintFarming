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

const plateAssessmentPrompt = `You are a safety classifier for an automated FDM 3D-printer farm. Inspect the printer's normal fixed-camera view and decide whether a printed part or other obstruction remains on the build plate before another print starts.

This camera angle normally does not show every edge or the full perimeter of the plate. Cropped plate edges, perspective, and the printer's toolhead, gantry, or other normal mechanism in the frame do not make the view unusable. Set plate_visible=true when the normal camera view is clear enough that a retained printed part would be visible. Set plate_visible=false and is_empty=false only when darkness, severe blur, a foreign occlusion, or another image problem prevents detecting a retained part in the normal observation area. Set is_empty=false if any printed part, loose object, substantial purge material, tool, hand, or other obstruction remains on the observed build surface. Normal surface texture, discoloration, and tiny harmless strands do not by themselves make a plate occupied. Never assume an ejection succeeded merely because this is a completion photo. Be conservative (is_empty=false) when uncertain whether a visible object remains, but do not demand a full top-down view of the entire plate.`

const firstLayerAssessmentPrompt = `You are a high-precision visual inspector for the first layer of an active FDM 3D print. False alarms are costly because a defective=true decision can pause a healthy print. Classify the layer as defective only when the image contains clear, direct visual evidence of a major first-layer failure that requires human intervention.

Major failure evidence includes unmistakably detached or lifted extrusion, loose filament being dragged into tangled paths, a substantial blob or clump, or broad regions of deposited lines that are visibly unbonded and displaced. Do not infer a defect merely from sparse or unfamiliar geometry because you do not know the intended model. Skirts, brims, purge lines, seams, intentional gaps, plate texture or markings, glare, shadows, and minor cosmetic inconsistency are not sufficient evidence. If the print area is cropped, obscured, dark, blurry, or the evidence is ambiguous, set first_layer_visible=false or is_defective=false. Use is_defective=true only when you are certain that a major physical first-layer failure is visibly present.`

type plateAssessment struct {
	PlateVisible bool    `json:"plate_visible"`
	IsEmpty      bool    `json:"is_empty"`
	Confidence   float64 `json:"confidence"`
	Reason       string  `json:"reason"`
}

type firstLayerAssessment struct {
	FirstLayerVisible bool    `json:"first_layer_visible"`
	IsDefective       bool    `json:"is_defective"`
	Confidence        float64 `json:"confidence"`
	Reason            string  `json:"reason"`
}

type openAIClient struct {
	apiKey      string
	baseURL     string
	model       string
	imageDetail string
	httpClient  *http.Client
}

func (c *openAIClient) assess(ctx context.Context, image []byte, mediaType string) (plateAssessment, error) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"plate_visible": map[string]any{
				"type":        "boolean",
				"description": "Whether the printer's normal fixed-camera view is usable for detecting a retained printed part; the full plate perimeter need not be visible.",
			},
			"is_empty": map[string]any{
				"type":        "boolean",
				"description": "Whether no printed part or other obstruction is visible on the observed build surface.",
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
	var assessment plateAssessment
	if err := c.assessImage(
		ctx,
		image,
		mediaType,
		plateAssessmentPrompt,
		"Classify this build plate.",
		"plate_assessment",
		schema,
		&assessment,
	); err != nil {
		return plateAssessment{}, err
	}
	if assessment.Confidence < 0 || assessment.Confidence > 1 {
		return plateAssessment{}, fmt.Errorf("OpenAI returned confidence outside 0..1")
	}
	if strings.TrimSpace(assessment.Reason) == "" {
		return plateAssessment{}, fmt.Errorf("OpenAI returned an empty assessment reason")
	}
	return assessment, nil
}

func (c *openAIClient) assessFirstLayer(ctx context.Context, image []byte, mediaType string) (firstLayerAssessment, error) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"first_layer_visible": map[string]any{
				"type":        "boolean",
				"description": "Whether enough of the deposited first layer is clearly visible to assess reliably.",
			},
			"is_defective": map[string]any{
				"type":        "boolean",
				"description": "Whether clear visual evidence proves a major physical first-layer failure requiring intervention.",
			},
			"confidence": map[string]any{
				"type":        "number",
				"minimum":     0,
				"maximum":     1,
				"description": "Confidence that the defective or non-defective classification is correct, from 0 to 1.",
			},
			"reason": map[string]any{
				"type":        "string",
				"description": "A short factual description of the visible evidence, without guessing the intended geometry.",
			},
		},
		"required":             []string{"first_layer_visible", "is_defective", "confidence", "reason"},
		"additionalProperties": false,
	}
	var assessment firstLayerAssessment
	if err := c.assessImage(
		ctx,
		image,
		mediaType,
		firstLayerAssessmentPrompt,
		"Inspect this active print's first layer for a certain major failure.",
		"first_layer_assessment",
		schema,
		&assessment,
	); err != nil {
		return firstLayerAssessment{}, err
	}
	if assessment.Confidence < 0 || assessment.Confidence > 1 {
		return firstLayerAssessment{}, fmt.Errorf("OpenAI returned first-layer confidence outside 0..1")
	}
	if strings.TrimSpace(assessment.Reason) == "" {
		return firstLayerAssessment{}, fmt.Errorf("OpenAI returned an empty first-layer assessment reason")
	}
	return assessment, nil
}

func (c *openAIClient) assessImage(
	ctx context.Context,
	image []byte,
	mediaType string,
	prompt string,
	requestText string,
	schemaName string,
	schema map[string]any,
	result any,
) error {
	imageURL := "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(image)
	payload := map[string]any{
		"model": c.model,
		"store": false,
		"input": []any{
			map[string]any{
				"role":    "developer",
				"content": prompt,
			},
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": requestText},
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
				"name":   schemaName,
				"strict": true,
				"schema": schema,
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode OpenAI request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/responses", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call OpenAI Responses API: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return responseError("OpenAI Responses API", resp)
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
		return fmt.Errorf("decode OpenAI response: %w", err)
	}
	if response.Error != nil {
		return fmt.Errorf("OpenAI response error: %s", response.Error.Message)
	}
	if response.Status != "completed" {
		return fmt.Errorf("OpenAI response status is %q", response.Status)
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
				return fmt.Errorf("OpenAI refused the image assessment: %s", content.Refusal)
			}
		}
	}
	if strings.TrimSpace(outputText) == "" {
		return fmt.Errorf("OpenAI response did not contain output text")
	}
	if err := json.Unmarshal([]byte(outputText), result); err != nil {
		return fmt.Errorf("decode OpenAI image assessment: %w", err)
	}
	return nil
}
