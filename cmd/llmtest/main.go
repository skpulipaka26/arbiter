package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

type ChatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type RequestConfig struct {
	Priority string
	Query    string
	ID       string
}

func main() {
	// Create requests with different priorities
	requests := []RequestConfig{
		{Priority: "low", Query: "Count from 1 to 3", ID: "LOW-1"},
		{Priority: "low", Query: "List 3 colors", ID: "LOW-2"},
		{Priority: "medium", Query: "Say hello", ID: "MEDIUM-1"},
		{Priority: "medium", Query: "Say goodbye", ID: "MEDIUM-2"},
		{Priority: "high", Query: "What is 2+2?", ID: "HIGH-1"},
		{Priority: "high", Query: "What is the capital of France?", ID: "HIGH-2"},
	}

	var wg sync.WaitGroup

	// Send all requests at once - truly simultaneously
	for _, req := range requests {
		wg.Add(1)
		go func(config RequestConfig) {
			defer wg.Done()
			sendLLMRequest(config)
		}(req)
	}

	// Wait for all requests to complete
	wg.Wait()
}

func sendLLMRequest(config RequestConfig) {
	// Create chat request
	chatReq := ChatRequest{
		Model: "qwen2.5-coder-7b-instruct",
		Messages: []Message{
			{Role: "user", Content: config.Query},
		},
		Stream: true, // Enable streaming
	}

	body, err := json.Marshal(chatReq)
	if err != nil {
		return
	}

	// Create HTTP request to Arbiter
	req, err := http.NewRequest("POST", "http://localhost:8080/v1/chat/completions", bytes.NewBuffer(body))
	if err != nil {
		return
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Priority", config.Priority)
	req.Header.Set("Accept", "text/event-stream")

	// Send request
	client := &http.Client{
		Timeout: 60 * time.Second,
	}

	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return
	}

	firstToken := true
	scanner := bufio.NewScanner(resp.Body)

	for scanner.Scan() {
		line := scanner.Text()

		// SSE format: data: {...}
		if len(line) > 6 && line[:6] == "data: " {
			data := line[6:]

			// Check for end of stream
			if data == "[DONE]" {
				fmt.Printf("\n")
				break
			}

			// Parse JSON chunk
			var chunk map[string]any
			if err := json.Unmarshal([]byte(data), &chunk); err == nil {
				// Extract content from the chunk
				if choices, ok := chunk["choices"].([]any); ok && len(choices) > 0 {
					if choice, ok := choices[0].(map[string]any); ok {
						if delta, ok := choice["delta"].(map[string]any); ok {
							if content, ok := delta["content"].(string); ok {
								// Print header and question only on first actual token
								if firstToken {
									fmt.Printf("\n[%s-%s] <%s> ", config.Priority, config.ID, config.Query)
									firstToken = false
								}
								// Print each token as it arrives
								fmt.Print(content)
							}
						}
					}
				}
			}
		}
	}
}
