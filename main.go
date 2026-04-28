package main

import (
	"bufio"
	"encoding/json"
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
	if err != nil { return }
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) == 0 || strings.HasPrefix(line, "#") { continue }
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			os.Setenv(strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]))
		}
	}
}

func getEnvConfig() Config {
	loadDotEnv(".env")
	slaveID, _ := strconv.Atoi(os.Getenv("SLAVE_ID"))
	pollSec, _ := strconv.Atoi(os.Getenv("POLLING_INTERVAL_SECONDS"))
	if pollSec <= 0 { pollSec = 1 }
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
		log.Fatalf("[FATAL] %v", err)
	}
	defer handler.Close()
	client := modbus.NewClient(handler)

	go func() {
		ticker := time.NewTicker(config.PollingInterval)
		for range ticker.C {
			// DEBUG: Coba baca 10 register sekaligus mulai dari alamat 0
			// Untuk melihat di mana sebenarnya data Anda berada
			regs, err := client.ReadHoldingRegisters(0, 10)
			if err != nil {
				log.Printf("[ERROR] Modbus Read Error: %v", err)
				continue
			}

			// Tampilkan 10 register pertama untuk analisa manual
			var debugStr []string
			for i := 0; i < 10; i++ {
				val := uint16(regs[i*2])<<8 | uint16(regs[i*2+1])
				debugStr = append(debugStr, fmt.Sprintf("MW%d:%d", i, val))
			}
			log.Printf("[DEBUG RAW] %s", strings.Join(debugStr, " | "))

			// Kita gunakan MW0, MW1, MW2 (sesuai screenshot)
			// Namun jika MW0-2 di debug nilainya tetap 0, perhatikan hasil debug MW3-9
			parseVal := func(b []byte, off int) (float64, bool) {
				raw := uint16(b[off])<<8 | uint16(b[off+1])
				if raw == 0 { return 0, true }
				if raw > 1000 {
					val := (float64(raw) - 4000) / 160.0
					if val < 0 { val = 0 }
					if val > 100 { val = 100 }
					return val, false
				}
				return float64(raw), false
			}

			mu.Lock()
			dataStore.Gas1, dataStore.Fault1 = parseVal(regs, 0) // MW0
			dataStore.Gas2, dataStore.Fault2 = parseVal(regs, 2) // MW1
			dataStore.Gas3, dataStore.Fault3 = parseVal(regs, 4) // MW2
			dataStore.LastUpdate = time.Now().Format("15:04:05")
			currentData := dataStore
			mu.Unlock()

			for i := 0; i < 3; i++ {
				var val float64
				var f bool
				var dID string
				switch i {
				case 0: val, f, dID = currentData.Gas1, currentData.Fault1, config.DeviceMapping[0]
				case 1: val, f, dID = currentData.Gas2, currentData.Fault2, config.DeviceMapping[1]
				case 2: val, f, dID = currentData.Gas3, currentData.Fault3, config.DeviceMapping[2]
				}
				if dID == "" { continue }
				
				sendVal := val
				if f { 
					sendVal = -1.0 
					log.Printf("[LOG] Sensor %d (ID %s): KABEL PUTUS (MW=%d)", i+1, dID, 0)
				} else {
					log.Printf("[LOG] Sensor %d (ID %s): %.2f", i+1, dID, val)
				}
				go forwardToAPI(config.APIBaseURL, dID, sendVal)
			}
		}
	}()

	http.HandleFunc("/api/data", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		json.NewEncoder(w).Encode(dataStore)
		mu.Unlock()
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<html><head><title>Dashboard</title></head><body style="background:#1a1a1a;color:white;font-family:sans-serif;text-align:center">
		<h1>Gas Debug Dashboard</h1><div id="d"></div><pre id="raw" style="color:#888;margin-top:20px"></pre>
		<script>
			async function u(){
				const res=await fetch('/api/data');const d=await res.json();
				document.getElementById('d').innerHTML = JSON.stringify(d, null, 2);
			}setInterval(u,1000);u();
		</script></body></html>`)
	})

	log.Printf("[INFO] Server di :8080. Cek log di terminal untuk [DEBUG RAW]")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func forwardToAPI(baseURL, deviceID string, value float64) {
	url := fmt.Sprintf("%s?device_id=%s&value=%.2f", baseURL, deviceID, value)
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err == nil { resp.Body.Close() }
}
