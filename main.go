package main

import (
	"bufio"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/goburrow/modbus"
)

// Config holds the application configuration
type Config struct {
	ModbusAddr      string
	SlaveID         byte
	APIBaseURL      string
	PollingInterval time.Duration
	DeviceMapping   map[int]string
}

func loadDotEnv(filename string) {
	file, err := os.Open(filename)
	if err != nil {
		log.Printf("[INFO] No %s file found, using system environment variables", filename)
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) == 0 || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			os.Setenv(key, val)
		}
	}
}

func getEnvConfig() Config {
	loadDotEnv(".env")

	slaveID, _ := strconv.Atoi(os.Getenv("SLAVE_ID"))
	pollSec, _ := strconv.Atoi(os.Getenv("POLLING_INTERVAL_SECONDS"))
	if pollSec <= 0 {
		pollSec = 1
	}

	return Config{
		ModbusAddr:      os.Getenv("MODBUS_ADDR"),
		SlaveID:         byte(slaveID),
		APIBaseURL:      os.Getenv("API_BASE_URL"),
		PollingInterval: time.Duration(pollSec) * time.Second,
		DeviceMapping: map[int]string{
			0: os.Getenv("DEVICE_ID_GAS_1"),
			1: os.Getenv("DEVICE_ID_GAS_2"),
			2: os.Getenv("DEVICE_ID_GAS_3"),
		},
	}
}

func main() {
	config := getEnvConfig()

	// 1. Modbus TCP Client Configuration
	handler := modbus.NewTCPClientHandler(config.ModbusAddr)
	handler.Timeout = 5 * time.Second
	handler.SlaveId = config.SlaveID

	err := handler.Connect()
	if err != nil {
		log.Fatalf("[FATAL] Failed to connect to Modbus PLC at %s: %v", config.ModbusAddr, err)
	}
	defer func() {
		handler.Close()
		log.Println("[INFO] Modbus connection closed.")
	}()

	client := modbus.NewClient(handler)

	log.Printf("[INFO] Starting Gas Sensor Monitoring System")
	log.Printf("[INFO] Target PLC: %s (Slave ID: %d)", config.ModbusAddr, config.SlaveID)

	// Graceful shutdown channel
	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, os.Interrupt, syscall.SIGTERM)

	ticker := time.NewTicker(config.PollingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-stopChan:
			log.Println("[INFO] Shutting down application...")
			return
		case <-ticker.C:
			// 2. Read 3 Holding Registers starting at 0 (%MW0 - %MW2)
			results, err := client.ReadHoldingRegisters(0, 3)
			if err != nil {
				log.Printf("[ERROR] Modbus Read Registers Error: %v", err)
				continue
			}

			// 2.1 Read 3 Discrete Inputs starting at 30 (%M30 - %M32) for Fault status
			coils, err2 := client.ReadDiscreteInputs(30, 3)
			if err2 != nil {
				log.Printf("[ERROR] Modbus Read Coils/Inputs Error: %v", err2)
				continue
			}

			// 3. Parse and Forward Data
			for i := 0; i < 3; i++ {
				// Parse register value
				valRaw := uint16(results[i*2])<<8 | uint16(results[i*2+1])
				valFloat := float64(valRaw)

				// Parse fault status (coil)
				// coils[0] contains the first 8 coils as bits
				isFault := (coils[0] & (1 << i)) != 0
				
				// Logic: if fault detected (kabel putus), send -1 to API
				if isFault {
					valFloat = -1.0
				}

				deviceID, ok := config.DeviceMapping[i]
				if !ok || deviceID == "" {
					continue
				}

				// Log the reading to terminal
				if isFault {
					log.Printf("[FAULT] Sensor %d (ID %s): KABEL PUTUS, sending -1.00", i+1, deviceID)
				} else {
					log.Printf("[DATA] Sensor %d (ID %s): Raw=%d, Float=%.2f", i+1, deviceID, valRaw, valFloat)
				}

				// 4. Forward to API asynchronously (Goroutine)
				go forwardToAPI(config.APIBaseURL, deviceID, valFloat)
			}
		}
	}
}

func forwardToAPI(baseURL, deviceID string, value float64) {
	// Construct the URL
	url := fmt.Sprintf("%s?device_id=%s&value=%.2f", baseURL, deviceID, value)

	// HTTP Client with Timeout
	httpClient := &http.Client{
		Timeout: 3 * time.Second,
	}

	resp, err := httpClient.Get(url)
	if err != nil {
		log.Printf("[API Error] Device %s: %v", deviceID, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		log.Printf("[API Success] Device %s: value %.2f sent", deviceID, value)
	} else {
		log.Printf("[API Failure] Device %s: status %s", deviceID, resp.Status)
	}
}
