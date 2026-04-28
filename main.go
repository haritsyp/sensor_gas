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

	handler := modbus.NewTCPClientHandler(config.ModbusAddr)
	handler.Timeout = 5 * time.Second
	handler.SlaveId = config.SlaveID

	err := handler.Connect()
	if err != nil {
		log.Fatalf("[FATAL] Failed to connect to Modbus PLC at %s: %v", config.ModbusAddr, err)
	}
	defer handler.Close()

	client := modbus.NewClient(handler)

	log.Printf("[INFO] Starting Gas Sensor Monitoring System")

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
			// 1. Read Gas Values (%MW0 - %MW2)
			results, err := client.ReadHoldingRegisters(0, 3)
			if err != nil {
				log.Printf("[ERROR] Modbus Gas Read Error: %v", err)
				continue
			}

			// 2. Read Fault Status (Alternative Trial)
			// Karena Function 01 & 02 ditolak, kita inisialisasi false
			faults := []bool{false, false, false}

			// Coba baca dari Discrete Inputs (Function 02)
			coils, err2 := client.ReadDiscreteInputs(30, 3)
			if err2 == nil {
				faults[0] = (coils[0] & 0x01) != 0
				faults[1] = (coils[0] & 0x02) != 0
				faults[2] = (coils[0] & 0x04) != 0
			} else {
				// Jika gagal lagi, kita diamkan saja agar tidak memenuhi log
				// atau coba baca sebagai Holding Register 30 jika memang di sana tempatnya
				regFault, err3 := client.ReadHoldingRegisters(30, 1)
				if err3 == nil && len(regFault) >= 2 {
					val := uint16(regFault[0])<<8 | uint16(regFault[1])
					faults[0] = (val & 0x01) != 0
					faults[1] = (val & 0x02) != 0
					faults[2] = (val & 0x04) != 0
				}
			}

			// 3. Forward Data
			for i := 0; i < 3; i++ {
				valRaw := uint16(results[i*2])<<8 | uint16(results[i*2+1])
				valFloat := float64(valRaw)

				if faults[i] {
					valFloat = -1.0
				}

				deviceID := config.DeviceMapping[i]
				if deviceID == "" {
					continue
				}

				if faults[i] {
					log.Printf("[FAULT] Sensor %d (ID %s): KABEL PUTUS", i+1, deviceID)
				} else {
					log.Printf("[DATA] Sensor %d (ID %s): %.2f", i+1, deviceID, valFloat)
				}

				go forwardToAPI(config.APIBaseURL, deviceID, valFloat)
			}
		}
	}
}

func forwardToAPI(baseURL, deviceID string, value float64) {
	url := fmt.Sprintf("%s?device_id=%s&value=%.2f", baseURL, deviceID, value)
	log.Println(url)
	httpClient := &http.Client{Timeout: 3 * time.Second}
	resp, err := httpClient.Get(url)
	if err != nil {
		return
	}
	defer resp.Body.Close()
}
