package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type TestResult struct {
	ID           int           `json:"id"`
	Priority     string        `json:"priority"`
	StatusCode   int           `json:"status_code"`
	ResponseTime time.Duration `json:"response_time"`
	Success      bool          `json:"success"`
	StartTime    time.Time     `json:"start_time"`
	Error        string        `json:"error,omitempty"`
}

type LoadTestConfig struct {
	TotalRequests   int
	ConcurrentUsers int
	ArbiterURL      string
}

type TestStats struct {
	TotalRequests     int                      `json:"total_requests"`
	SuccessfulReqs    int                      `json:"successful_requests"`
	FailedReqs        int                      `json:"failed_requests"`
	TestDuration      time.Duration            `json:"test_duration"`
	RequestsPerSec    float64                  `json:"requests_per_second"`
	PriorityStats     map[string]PriorityStats `json:"priority_stats"`
	ErrorDistribution map[string]int           `json:"error_distribution"`
}

type PriorityStats struct {
	Count           int           `json:"count"`
	SuccessCount    int           `json:"success_count"`
	SuccessRate     float64       `json:"success_rate"`
	AvgResponseTime time.Duration `json:"avg_response_time"`
	MinResponseTime time.Duration `json:"min_response_time"`
	MaxResponseTime time.Duration `json:"max_response_time"`
	P95ResponseTime time.Duration `json:"p95_response_time"`
	P99ResponseTime time.Duration `json:"p99_response_time"`
}


func generatePriority() string {
	r := rand.Float64()
	switch {
	case r < 0.1: // 10% high priority
		return "high"
	case r < 0.3: // 20% medium priority
		return "medium"
	default: // 70% low priority
		return "low"
	}
}


func sendRequest(id int, config *LoadTestConfig) TestResult {
	startTime := time.Now()

	priority := generatePriority()

	payload := map[string]interface{}{
		"test_id":   id,
		"message":   fmt.Sprintf("Load test request %d", id),
		"timestamp": startTime.Unix(),
		"priority":  priority,
	}

	payloadBytes, _ := json.Marshal(payload)

	// HTTPBin has /post, /get, /anything (accepts all methods)
	// Using /anything as it accepts all HTTP methods and returns the request data
	endpoint := "/anything"

	req, err := http.NewRequest("POST", config.ArbiterURL+endpoint, bytes.NewBuffer(payloadBytes))
	if err != nil {
		return TestResult{
			ID:           id,
			Priority:     priority,
			ResponseTime: time.Since(startTime),
			Success:      false,
			StartTime:    startTime,
			Error:        fmt.Sprintf("Request creation failed: %v", err),
		}
	}

	req.Header.Set("Priority", priority)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Arbiter-LoadTest/1.0")

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	responseTime := time.Since(startTime)

	result := TestResult{
		ID:           id,
		Priority:     priority,
		ResponseTime: responseTime,
		StartTime:    startTime,
		Success:      false,
	}

	if err != nil {
		result.Error = fmt.Sprintf("HTTP error: %v", err)
		return result
	}
	defer resp.Body.Close()

	result.StatusCode = resp.StatusCode
	result.Success = resp.StatusCode >= 200 && resp.StatusCode < 300

	if !result.Success {
		result.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}

	return result
}

func runLoadTest(config *LoadTestConfig) []TestResult {
	results := make([]TestResult, 0, config.TotalRequests)
	resultsChan := make(chan TestResult, config.TotalRequests)
	var wg sync.WaitGroup

	requestChan := make(chan int, config.TotalRequests)

	for i := 0; i < config.ConcurrentUsers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for requestID := range requestChan {
				result := sendRequest(requestID, config)
				resultsChan <- result
			}
		}(i)
	}

	go func() {
		for i := 1; i <= config.TotalRequests; i++ {
			requestChan <- i
		}
		close(requestChan)
	}()

	go func() {
		wg.Wait()
		close(resultsChan)
	}()

	for result := range resultsChan {
		results = append(results, result)
	}

	return results
}

func analyzeResults(results []TestResult) TestStats {
	if len(results) == 0 {
		return TestStats{}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].StartTime.Before(results[j].StartTime)
	})

	testDuration := results[len(results)-1].StartTime.Sub(results[0].StartTime)
	if testDuration == 0 {
		testDuration = time.Second
	}

	stats := TestStats{
		TotalRequests:     len(results),
		TestDuration:      testDuration,
		RequestsPerSec:    float64(len(results)) / testDuration.Seconds(),
		PriorityStats:     make(map[string]PriorityStats),
		ErrorDistribution: make(map[string]int),
	}

	priorityResults := make(map[string][]TestResult)

	for _, result := range results {
		if result.Success {
			stats.SuccessfulReqs++
		} else {
			stats.FailedReqs++
			stats.ErrorDistribution[result.Error]++
		}

		priorityResults[result.Priority] = append(priorityResults[result.Priority], result)
	}

	for priority, reqs := range priorityResults {
		stats.PriorityStats[priority] = calculateStats(reqs)
	}

	return stats
}

func calculateStats(results []TestResult) PriorityStats {
	if len(results) == 0 {
		return PriorityStats{}
	}

	successCount := 0
	var responseTimes []time.Duration
	var totalTime time.Duration
	minTime := results[0].ResponseTime
	maxTime := results[0].ResponseTime

	for _, result := range results {
		if result.Success {
			successCount++
			responseTimes = append(responseTimes, result.ResponseTime)
			totalTime += result.ResponseTime

			if result.ResponseTime < minTime {
				minTime = result.ResponseTime
			}
			if result.ResponseTime > maxTime {
				maxTime = result.ResponseTime
			}
		}
	}

	stats := PriorityStats{
		Count:           len(results),
		SuccessCount:    successCount,
		SuccessRate:     float64(successCount) / float64(len(results)) * 100,
		MinResponseTime: minTime,
		MaxResponseTime: maxTime,
	}

	if len(responseTimes) > 0 {
		sort.Slice(responseTimes, func(i, j int) bool {
			return responseTimes[i] < responseTimes[j]
		})

		stats.AvgResponseTime = totalTime / time.Duration(len(responseTimes))
		stats.P95ResponseTime = responseTimes[int(float64(len(responseTimes))*0.95)]
		stats.P99ResponseTime = responseTimes[int(float64(len(responseTimes))*0.99)]
	}

	return stats
}

func printResults(stats TestStats) {
	fmt.Printf("\n" + strings.Repeat("=", 80) + "\n")
	fmt.Printf("ARBITER LOAD TEST RESULTS\n")
	fmt.Printf(strings.Repeat("=", 80) + "\n")

	fmt.Printf("OVERALL PERFORMANCE:\n")
	fmt.Printf("  Total Requests:      %d\n", stats.TotalRequests)
	fmt.Printf("  Successful:          %d (%.1f%%)\n",
		stats.SuccessfulReqs, float64(stats.SuccessfulReqs)/float64(stats.TotalRequests)*100)
	fmt.Printf("  Failed:              %d (%.1f%%)\n",
		stats.FailedReqs, float64(stats.FailedReqs)/float64(stats.TotalRequests)*100)
	fmt.Printf("  Test Duration:       %v\n", stats.TestDuration)
	fmt.Printf("  Requests/sec:        %.2f\n", stats.RequestsPerSec)

	fmt.Printf("\nPRIORITY BREAKDOWN:\n")
	fmt.Printf(strings.Repeat("-", 80) + "\n")
	priorities := []string{"high", "medium", "low"}
	for _, priority := range priorities {
		if pstats, exists := stats.PriorityStats[priority]; exists {
			fmt.Printf("  %s Priority:\n", strings.Title(priority))
			fmt.Printf("    Requests:          %d\n", pstats.Count)
			fmt.Printf("    Success Rate:      %.1f%%\n", pstats.SuccessRate)
			fmt.Printf("    Avg Response Time: %v\n", pstats.AvgResponseTime)
			fmt.Printf("    95th Percentile:   %v\n", pstats.P95ResponseTime)
			fmt.Printf("    99th Percentile:   %v\n", pstats.P99ResponseTime)
			fmt.Printf("\n")
		}
	}

	if len(stats.ErrorDistribution) > 0 {
		fmt.Printf("ERROR BREAKDOWN:\n")
		fmt.Printf(strings.Repeat("-", 80) + "\n")
		for error, count := range stats.ErrorDistribution {
			fmt.Printf("  %s: %d\n", error, count)
		}
		fmt.Printf("\n")
	}

	highStats := stats.PriorityStats["high"]
	mediumStats := stats.PriorityStats["medium"]
	lowStats := stats.PriorityStats["low"]

	if highStats.SuccessCount > 0 && mediumStats.SuccessCount > 0 && lowStats.SuccessCount > 0 {
		fmt.Printf("PRIORITY EFFECTIVENESS:\n")
		fmt.Printf(strings.Repeat("-", 80) + "\n")
		fmt.Printf("  High vs Medium:    %.2fx faster\n",
			float64(mediumStats.AvgResponseTime)/float64(highStats.AvgResponseTime))
		fmt.Printf("  High vs Low:       %.2fx faster\n",
			float64(lowStats.AvgResponseTime)/float64(highStats.AvgResponseTime))
		fmt.Printf("  Medium vs Low:     %.2fx faster\n",
			float64(lowStats.AvgResponseTime)/float64(mediumStats.AvgResponseTime))
		fmt.Printf("\n")

		if highStats.AvgResponseTime <= mediumStats.AvgResponseTime &&
			mediumStats.AvgResponseTime <= lowStats.AvgResponseTime {
			fmt.Printf("PRIORITY SYSTEM WORKING CORRECTLY!\n")
			fmt.Printf("   High ≤ Medium ≤ Low response times\n")
		} else {
			fmt.Printf("Priority ordering issues detected\n")
		}
		fmt.Printf("\n")
	}

	fmt.Printf(strings.Repeat("=", 80) + "\n")
}

func main() {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("http://localhost:8080/health")
	if err != nil {
		log.Fatalf("Arbiter not running: %v\n", err)
	}
	resp.Body.Close()

	if resp.StatusCode != 200 {
		log.Fatalf("Arbiter not healthy: status %d\n", resp.StatusCode)
	}

	config := &LoadTestConfig{
		TotalRequests:   5000,
		ConcurrentUsers: 100,
		ArbiterURL:      "http://localhost:8080",
	}

	results := runLoadTest(config)

	stats := analyzeResults(results)
	printResults(stats)

}
