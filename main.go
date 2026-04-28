package main

import (
	"bufio"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/goburrow/modbus"
)

type GasData struct {
	Gas1       float64 `json:"gas1"`
	Gas2       float64 `json:"gas2"`
	Gas3       float64 `json:"gas3"`
	Fault1     bool    `json:"fault1"`
	Fault2     bool    `json:"fault2"`
	Fault3     bool    `json:"fault3"`
	LastUpdate string  `json:"last_update"`
}

var (
	dataStore GasData
	mu        sync.Mutex
)

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
			os.Setenv(strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]))
		}
	}
}

func getEnvConfig() Config {
	loadDotEnv(".env")
	sID := 1
	pollSec, _ := strconv.Atoi(os.Getenv("POLLING_INTERVAL_SECONDS"))
	if pollSec <= 0 {
		pollSec = 1
	}
	return Config{
		ModbusAddr:      os.Getenv("MODBUS_ADDR"),
		SlaveID:         byte(sID),
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
		log.Fatalf("[FATAL] %v", err)
	}
	defer handler.Close()
	client := modbus.NewClient(handler)

	log.Printf("[INFO] Searching Analog Values (IW0-5 or MW10-20) | Slave ID: %d", config.SlaveID)

	go func() {
		ticker := time.NewTicker(config.PollingInterval)
		for range ticker.C {
			// 1. Ambil Status Fault dari MW0-2 (Holding Register)
			statusRegs, errS := client.ReadHoldingRegisters(0, 3)

			// 2. COBA: Baca Nilai Analog dari Input Register 0 (Function 04)
			// Schneider M221 terkadang meletakkan %IW di Input Register 0
			analogRegs, errA := client.ReadInputRegisters(0, 5)

			// 3. ALTERNATIF: Jika IR gagal, scan MW10-15 (Holding Register)
			var analogData []byte
			dataSource := "INPUT_REG_0"

			if errA == nil {
				analogData = analogRegs
			} else {
				// Coba baca Holding Register 10 (mungkin nilai analog di-copy ke sini)
				mwScan, errM := client.ReadHoldingRegisters(10, 5)
				if errM == nil {
					analogData = mwScan
					dataSource = "HOLDING_REG_10"
				}
			}

			if errS == nil {
				mu.Lock()
				// Update Status Fault
				dataStore.Fault1 = (uint16(statusRegs[0])<<8 | uint16(statusRegs[1])) == 0
				dataStore.Fault2 = (uint16(statusRegs[2])<<8 | uint16(statusRegs[3])) == 0
				dataStore.Fault3 = (uint16(statusRegs[4])<<8 | uint16(statusRegs[5])) == 0

				// Update Nilai Gas (Jika data analog ditemukan)
				if len(analogData) >= 6 {
					v1 := uint16(analogData[0])<<8 | uint16(analogData[1])
					v2 := uint16(analogData[2])<<8 | uint16(analogData[3])
					v3 := uint16(analogData[4])<<8 | uint16(analogData[5])

					log.Printf("[DEBUG] Source:%s | Raw1:%d | Raw2:%d | Raw3:%d", dataSource, v1, v2, v3)

					// Konversi 4000-20000 ke 0-100% (opsional, jika ingin tetap raw, hapus rumus ini)
					dataStore.Gas1 = float64(v1)
					dataStore.Gas2 = float64(v2)
					dataStore.Gas3 = float64(v3)
				} else {
					// Fallback ke status 0/1 jika analog tidak ketemu
					dataStore.Gas1 = float64(uint16(statusRegs[0])<<8 | uint16(statusRegs[1]))
					dataStore.Gas2 = float64(uint16(statusRegs[2])<<8 | uint16(statusRegs[3]))
					dataStore.Gas3 = float64(uint16(statusRegs[4])<<8 | uint16(statusRegs[5]))
				}

				dataStore.LastUpdate = time.Now().Format("15:04:05")
				mu.Unlock()
			}

			// Forward data ke API
			mu.Lock()
			current := dataStore
			mu.Unlock()

			for i := 0; i < 3; i++ {
				var val float64
				var f bool
				var dID string
				switch i {
				case 0:
					val, f, dID = current.Gas1, current.Fault1, config.DeviceMapping[0]
				case 1:
					val, f, dID = current.Gas2, current.Fault2, config.DeviceMapping[1]
				case 2:
					val, f, dID = current.Gas3, current.Fault3, config.DeviceMapping[2]
				}
				if dID == "" {
					continue
				}

				sendVal := val
				if f {
					sendVal = -1.0
				}
				go forwardToAPI(config.APIBaseURL, dID, sendVal)
			}
		}
	}()

	http.ListenAndServe(":8080", nil)
}

func forwardToAPI(baseURL, deviceID string, value float64) {
	url := fmt.Sprintf("%s?device_id=%s&value=%.2f", baseURL, deviceID, value)
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err == nil {
		resp.Body.Close()
	}
}
