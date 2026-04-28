package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/goburrow/modbus"
)

// GasData adalah struktur data untuk response API & Dashboard
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

	// 1. Goroutine untuk Polling Modbus
	go func() {
		ticker := time.NewTicker(config.PollingInterval)
		for range ticker.C {
			// Membaca %MW0 - %MW2 (3 register)
			results, err := client.ReadHoldingRegisters(0, 3)
			if err != nil {
				log.Printf("[ERROR] Modbus Read Error: %v", err)
				continue
			}

			// Parsing nilai register
			v1 := uint16(results[0])<<8 | uint16(results[1])
			v2 := uint16(results[2])<<8 | uint16(results[3])
			v3 := uint16(results[4])<<8 | uint16(results[5])

			mu.Lock()
			// Simpan nilai asli ke datastore
			dataStore.Gas1 = float64(v1)
			dataStore.Gas2 = float64(v2)
			dataStore.Gas3 = float64(v3)

			// Logika Patch: Berdasarkan screenshot, jika %MW = 0 artinya kabel putus (Fault)
			dataStore.Fault1 = (v1 == 0)
			dataStore.Fault2 = (v2 == 0)
			dataStore.Fault3 = (v3 == 0)
			
			dataStore.LastUpdate = time.Now().Format("15:04:05")
			currentData := dataStore
			mu.Unlock()

			// Log & Forward ke API
			for i := 0; i < 3; i++ {
				var val float64
				var isFault bool
				var dID string

				switch i {
				case 0: val, isFault, dID = currentData.Gas1, currentData.Fault1, config.DeviceMapping[0]
				case 1: val, isFault, dID = currentData.Gas2, currentData.Fault2, config.DeviceMapping[1]
				case 2: val, isFault, dID = currentData.Gas3, currentData.Fault3, config.DeviceMapping[2]
				}

				if dID == "" { continue }

				// Kirim -1 ke API jika fault
				sendVal := val
				if isFault {
					sendVal = -1.0
					log.Printf("[FAULT] Sensor %d (ID %s): KABEL PUTUS", i+1, dID)
				} else {
					log.Printf("[DATA] Sensor %d (ID %s): %.2f", i+1, dID, val)
				}
				go forwardToAPI(config.APIBaseURL, dID, sendVal)
			}
		}
	}()

	// 2. Web Server Handlers
	http.HandleFunc("/api/data", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		mu.Lock()
		json.NewEncoder(w).Encode(dataStore)
		mu.Unlock()
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `
			<html>
			<head>
				<title>Gas Monitoring Dashboard</title>
				<style>
					body { font-family: 'Segoe UI', Tahoma, Geneva, Verdana, sans-serif; text-align: center; background: #1a1a1a; color: white; }
					.container { display: flex; justify-content: center; flex-wrap: wrap; margin-top: 50px; }
					.card { background: #2d2d2d; padding: 25px; margin: 15px; border-radius: 15px; width: 250px; box-shadow: 0 10px 20px rgba(0,0,0,0.5); border-top: 5px solid #4CAF50; }
					.card.fault { border-top: 5px solid #f44336; }
					h3 { color: #aaa; margin-bottom: 5px; }
					h1 { font-size: 3.5em; margin: 10px 0; }
					.status { font-weight: bold; padding: 5px 10px; border-radius: 5px; }
					.ok { background: #2e7d32; }
					.error { background: #c62828; animation: blink 1s infinite; }
					@keyframes blink { 0%% { opacity: 1; } 50%% { opacity: 0.5; } 100%% { opacity: 1; } }
					.footer { margin-top: 30px; color: #666; }
				</style>
			</head>
			<body>
				<h1>Gas Monitoring System (Patched)</h1>
				<div id="dashboard" class="container">Loading Dashboard...</div>
				<div class="footer">Update Terakhir: <span id="time">-</span></div>
				<script>
					async function update() {
						try {
							const res = await fetch('/api/data');
							const d = await res.json();
							const createCard = (num, val, fault) => ` + "`" + `
								<div class="card ${fault ? 'fault' : ''}">
									<h3>Sensor ${num}</h3>
									<h1>${fault ? 'ERR' : val}</h1>
									<span class="status ${fault ? 'error' : 'ok'}">${fault ? 'KABEL PUTUS' : 'SISTEM OK'}</span>
								</div>
							` + "`" + `;
							
							document.getElementById('dashboard').innerHTML = 
								createCard(1, d.gas1, d.fault1) + 
								createCard(2, d.gas2, d.fault2) + 
								createCard(3, d.gas3, d.fault3);
							document.getElementById('time').innerText = d.last_update;
						} catch (e) { console.error(e); }
					}
					setInterval(update, 1000);
					update();
				</script>
			</body>
			</html>
		`)
	})

	port := ":8080"
	fmt.Printf("[INFO] Web Server berjalan di http://localhost%s\n", port)
	
	go func() {
		stopChan := make(chan os.Signal, 1)
		signal.Notify(stopChan, os.Interrupt, syscall.SIGTERM)
		<-stopChan
		log.Println("[INFO] Shutting down...")
		os.Exit(0)
	}()

	log.Fatal(http.ListenAndServe(port, nil))
}

func forwardToAPI(baseURL, deviceID string, value float64) {
	url := fmt.Sprintf("%s?device_id=%s&value=%.2f", baseURL, deviceID, value)
	httpClient := &http.Client{Timeout: 3 * time.Second}
	resp, err := httpClient.Get(url)
	if err != nil {
		return
	}
	defer resp.Body.Close()
}
