// Copyright 2024 Redpanda Data, Inc.
//
// Licensed as a Redpanda Enterprise file under the Redpanda Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
// https://github.com/redpanda-data/connect/blob/main/licenses/rcl.md

package ollama

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/ollama/ollama/api"
	"github.com/redpanda-data/benthos/v4/public/bloblang"
	"github.com/redpanda-data/benthos/v4/public/service"
)

const (
	ocpFieldUserPrompt     = "prompt"
	ocpFieldSystemPrompt   = "system_prompt"
	ocpFieldResponseFormat = "response_format"
	ocpFieldImage          = "image"
	// Prediction options
	ocpFieldMaxTokens          = "max_tokens"
	ocpFieldNumKeep            = "num_keep"
	ocpFieldSeed               = "seed"
	ocpFieldTopK               = "top_k"
	ocpFieldTopP               = "top_p"
	ocpFieldTemp               = "temperature"
	ocpFieldRepeatPenalty      = "repeat_penalty"
	ocpFieldPresencePenalty    = "presence_penalty"
	ocpFieldFrequencyPenalty   = "frequency_penalty"
	ocpFieldStop               = "stop"
	ocpFieldEmitPromptMetadata = "save_prompt_metadata"
)

func init() {
	err := service.RegisterProcessor(
		"ollama_chat",
		ollamaChatProcessorConfig(),
		makeOllamaCompletionProcessor,
	)
	if err != nil {
		panic(err)
	}
}

func ollamaChatProcessorConfig() *service.ConfigSpec {
	return service.NewConfigSpec().
		Categories("AI").
		Summary("Generates responses to messages in a chat conversation, using the Ollama API.").
		Description(`This processor sends prompts to your chosen Ollama large language model (LLM) and generates text from the responses, using the Ollama API.

By default, the processor starts and runs a locally installed Ollama server. Alternatively, to use an already running Ollama server, add your server details to the `+"`"+bopFieldServerAddress+"`"+` field. You can https://ollama.com/download[download and install Ollama from the Ollama website^].

For more information, see the https://github.com/ollama/ollama/tree/main/docs[Ollama documentation^].`).
		Version("4.32.0").
		Fields(
			service.NewStringField(bopFieldModel).
				Description("The name of the Ollama LLM to use. For a full list of models, see the https://ollama.com/models[Ollama website].").
				Examples("llama3.1", "gemma2", "qwen2", "phi3"),
			service.NewInterpolatedStringField(ocpFieldUserPrompt).
				Description("The prompt you want to generate a response for. By default, the processor submits the entire payload as a string.").
				Optional(),
			service.NewInterpolatedStringField(ocpFieldSystemPrompt).
				Description("The system prompt to submit to the Ollama LLM.").
				Advanced().
				Optional(),
			service.NewBloblangField(ocpFieldImage).
				Description("The image to submit along with the prompt to the model. The result should be a byte array.").
				Version("4.38.0").
				Optional().
				Example(`root = this.image.decode("base64") # decode base64 encoded image`),
			service.NewStringEnumField(ocpFieldResponseFormat, "text", "json").
				Description("The format of the response that the Ollama model generates. If specifying JSON output, then the `"+ocpFieldUserPrompt+"` should specify that the output should be in JSON as well.").
				Default("text"),
			service.NewIntField(ocpFieldMaxTokens).
				Optional().
				Description("The maximum number of tokens to predict and output. Limiting the amount of output means that requests are processed faster and have a fixed limit on the cost."),
			service.NewIntField(ocpFieldTemp).
				Optional().
				Description("The temperature of the model. Increasing the temperature makes the model answer more creatively.").
				LintRule(`root = if this > 2 || this < 0 { [ "field must be between 0.0 and 2.0" ] }`),
			service.NewIntField(ocpFieldNumKeep).
				Optional().
				Advanced().
				Description("Specify the number of tokens from the initial prompt to retain when the model resets its internal context. By default, this value is set to `4`. Use `-1` to retain all tokens from the initial prompt."),
			service.NewIntField(ocpFieldSeed).
				Optional().
				Advanced().
				Description("Sets the random number seed to use for generation. Setting this to a specific number will make the model generate the same text for the same prompt.").
				Example(42),
			service.NewIntField(ocpFieldTopK).
				Optional().
				Advanced().
				Description("Reduces the probability of generating nonsense. A higher value, for example `100`, will give more diverse answers. A lower value, for example `10`, will be more conservative."),
			service.NewFloatField(ocpFieldTopP).
				Optional().
				Advanced().
				Description("Works together with `top-k`. A higher value, for example 0.95, will lead to more diverse text. A lower value, for example 0.5, will generate more focused and conservative text.").
				LintRule(`root = if this > 1 || this < 0 { [ "field must be between 0.0 and 1.0" ] }`),
			service.NewFloatField(ocpFieldRepeatPenalty).
				Optional().
				Advanced().
				Description(`Sets how strongly to penalize repetitions. A higher value, for example 1.5, will penalize repetitions more strongly. A lower value, for example 0.9, will be more lenient.`).
				LintRule(`root = if this > 2 || this < -2 { [ "field must be between -2.0 and 2.0" ] }`),
			service.NewFloatField(ocpFieldPresencePenalty).
				Optional().
				Advanced().
				Description(`Positive values penalize new tokens if they have appeared in the text so far. This increases the model's likelihood to talk about new topics.`).
				LintRule(`root = if this > 2 || this < -2 { [ "field must be between -2.0 and 2.0" ] }`),
			service.NewFloatField(ocpFieldFrequencyPenalty).
				Optional().
				Advanced().
				Description(`Positive values penalize new tokens based on the frequency of their appearance in the text so far. This decreases the model's likelihood to repeat the same line verbatim.`).
				LintRule(`root = if this > 2 || this < -2 { [ "field must be between -2.0 and 2.0" ] }`),
			service.NewStringListField(ocpFieldStop).
				Optional().
				Advanced().
				Description(`Sets the stop sequences to use. When this pattern is encountered the LLM stops generating text and returns the final response.`),
			service.NewBoolField(ocpFieldEmitPromptMetadata).
				Default(false).
				Description(`If enabled the prompt is saved as @prompt metadata on the output message. If system_prompt is used it's also saved as @system_prompt`),
		).Fields(commonFields()...).
		Example(
			"Use Llava to analyze an image",
			"This example fetches image URLs from stdin and has a multimodal LLM describe the image.",
			`
input:
  stdin:
    scanner:
      lines: {}
pipeline:
  processors:
    - http:
        verb: GET
        url: "${!content().string()}"
    - ollama_chat:
        model: llava
        prompt: "Describe the following image"
        image: "root = content()"
output:
  stdout:
    codec: lines
`)
}

func makeOllamaCompletionProcessor(conf *service.ParsedConfig, mgr *service.Resources) (service.Processor, error) {
	p := ollamaCompletionProcessor{}
	if conf.Contains(ocpFieldUserPrompt) {
		pf, err := conf.FieldInterpolatedString(ocpFieldUserPrompt)
		if err != nil {
			return nil, err
		}
		p.userPrompt = pf
	}
	if conf.Contains(ocpFieldSystemPrompt) {
		pf, err := conf.FieldInterpolatedString(ocpFieldSystemPrompt)
		if err != nil {
			return nil, err
		}
		p.systemPrompt = pf
	}
	if conf.Contains(ocpFieldImage) {
		i, err := conf.FieldBloblang(ocpFieldImage)
		if err != nil {
			return nil, err
		}
		p.image = i
	}
	format, err := conf.FieldString(ocpFieldResponseFormat)
	if err != nil {
		return nil, err
	}
	if format == "json" {
		p.format = "json"
	} else if format == "text" {
		// This is the default
		p.format = ""
	} else {
		return nil, fmt.Errorf("invalid %s: %q", ocpFieldResponseFormat, format)
	}
	p.savePrompt, err = conf.FieldBool(ocpFieldEmitPromptMetadata)
	if err != nil {
		return nil, err
	}
	b, err := newBaseProcessor(conf, mgr)
	if err != nil {
		return nil, err
	}
	p.baseOllamaProcessor = b
	return &p, nil
}

type ollamaCompletionProcessor struct {
	*baseOllamaProcessor

	format       string
	userPrompt   *service.InterpolatedString
	systemPrompt *service.InterpolatedString
	image        *bloblang.Executor
	savePrompt   bool
}

func (o *ollamaCompletionProcessor) Process(ctx context.Context, msg *service.Message) (service.MessageBatch, error) {
	var sp string
	if o.systemPrompt != nil {
		p, err := o.systemPrompt.TryString(msg)
		if err != nil {
			return nil, err
		}
		sp = p
	}
	up, err := o.computePrompt(msg)
	if err != nil {
		return nil, err
	}
	var image []byte
	if o.image != nil {
		o, err := msg.BloblangQuery(o.image)
		if err != nil {
			return nil, fmt.Errorf("unable to execute bloblang for `%s`: %w", ocpFieldImage, err)
		}
		image, err = o.AsBytes()
		if err != nil {
			return nil, fmt.Errorf("unable to convert `%s` result to a byte array: %w", ocpFieldImage, err)
		}
	}
	g, err := o.generateCompletion(ctx, sp, up, image)
	if err != nil {
		return nil, err
	}
	m := msg.Copy()
	m.SetBytes([]byte(g))
	if o.savePrompt {
		if sp != "" {
			m.MetaSet("system_prompt", sp)
		}
		m.MetaSet("prompt", up)
	}
	return service.MessageBatch{m}, nil
}

func (o *ollamaCompletionProcessor) computePrompt(msg *service.Message) (string, error) {
	if o.userPrompt != nil {
		return o.userPrompt.TryString(msg)
	}
	b, err := msg.AsBytes()
	if err != nil {
		return "", err
	}
	if !utf8.Valid(b) {
		return "", errors.New("message payload contained invalid UTF8")
	}
	return string(b), nil
}

func (o *ollamaCompletionProcessor) generateCompletion(ctx context.Context, systemPrompt, userPrompt string, image []byte) (string, error) {
	var req api.ChatRequest
	req.Model = o.model
	req.Options = o.opts
	req.Format = o.format
	if systemPrompt != "" {
		req.Messages = append(req.Messages, api.Message{
			Role:    "system",
			Content: systemPrompt,
		})
	}
	var images []api.ImageData
	if image != nil {
		images = []api.ImageData{image}
	}
	req.Messages = append(req.Messages, api.Message{
		Role:    "user",
		Content: userPrompt,
		Images:  images,
	})
	shouldStream := false
	req.Stream = &shouldStream
	var g string
	err := o.client.Chat(ctx, &req, func(resp api.ChatResponse) error {
		g = resp.Message.Content
		return nil
	})
	return g, err
}

func (o *ollamaCompletionProcessor) Close(ctx context.Context) error {
	return o.baseOllamaProcessor.Close(ctx)
}
