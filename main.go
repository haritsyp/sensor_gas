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
		log.Fatalf("[FATAL] %v", err)
	}
	defer handler.Close()
	client := modbus.NewClient(handler)

	go func() {
		ticker := time.NewTicker(config.PollingInterval)
		for range ticker.C {
			// 1. Baca Nilai Analog dari %IW1.0 - %IW1.2 (Input Registers 10-12)
			// Jika alamat 10 salah, coba alamat 0 atau 1
			analogBytes, errA := client.ReadInputRegisters(10, 3)

			// 2. Baca Status dari %MW0 - %MW2 (Holding Registers 0-2)
			statusBytes, errS := client.ReadHoldingRegisters(0, 3)

			if errA != nil || errS != nil {
				log.Printf("[ERROR] Modbus: AnalogErr=%v, StatusErr=%v", errA, errS)
				continue
			}

			// Parsing Analog (%IW) - Skala 4000-20000 -> 0-100%
			parseAnalog := func(b []byte, off int) float64 {
				raw := float64(uint16(b[off])<<8 | uint16(b[off+1]))
				val := (raw - 4000) / 160.0
				if val < 0 {
					val = 0
				}
				if val > 100 {
					val = 100
				}
				return val
			}

			// Parsing Status (%MW) - 1=OK, 0=Fault
			isFault := func(b []byte, off int) bool {
				return (uint16(b[off])<<8 | uint16(b[off+1])) == 0
			}

			mu.Lock()
			dataStore.Gas1 = parseAnalog(analogBytes, 0)
			dataStore.Gas2 = parseAnalog(analogBytes, 2)
			dataStore.Gas3 = parseAnalog(analogBytes, 4)
			dataStore.Fault1 = isFault(statusBytes, 0)
			dataStore.Fault2 = isFault(statusBytes, 2)
			dataStore.Fault3 = isFault(statusBytes, 4)
			dataStore.LastUpdate = time.Now().Format("15:04:05")
			currentData := dataStore
			mu.Unlock()

			// Forward to API
			for i := 0; i < 3; i++ {
				var val float64
				var f bool
				var dID string
				switch i {
				case 0:
					val, f, dID = currentData.Gas1, currentData.Fault1, config.DeviceMapping[0]
				case 1:
					val, f, dID = currentData.Gas2, currentData.Fault2, config.DeviceMapping[1]
				case 2:
					val, f, dID = currentData.Gas3, currentData.Fault3, config.DeviceMapping[2]
				}
				if dID == "" {
					continue
				}

				sendVal := val
				if f {
					sendVal = -1.0
					log.Printf("[FAULT] Sensor %d (ID %s): KABEL PUTUS", i+1, dID)
				} else {
					log.Printf("[DATA] Sensor %d (ID %s): %.2f%%", i+1, dID, val)
				}
				go forwardToAPI(config.APIBaseURL, dID, sendVal)
			}
		}
	}()

	// Web Server
	http.HandleFunc("/api/data", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		json.NewEncoder(w).Encode(dataStore)
		mu.Unlock()
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<html><head><title>Dashboard</title><style>body{font-family:sans-serif;background:#1a1a1a;color:white;text-align:center}.card{background:#2d2d2d;padding:20px;margin:10px;display:inline-block;border-radius:10px;width:200px;border-top:5px solid #4CAF50}.card.fault{border-top-color:#f44336}.error{color:#f44336;font-weight:bold}</style></head>
		<body><h1>Gas Monitoring System</h1><div id="d"></div>
		<script>
			async function u(){
				const res=await fetch('/api/data');const d=await res.json();
				const c=(n,v,f)=>`+"`"+`<div class="card ${f?'fault':''}"><h3>Sensor ${n}</h3><h1>${f?'ERR':v+'%'}</h1><p class="${f?'error':''}">${f?'KABEL PUTUS':'OK'}</p></div>`+"`"+`;
				document.getElementById('d').innerHTML=c(1,d.gas1,d.fault1)+c(2,d.gas2,d.fault2)+c(3,d.gas3,d.fault3);
			}setInterval(u,1000);u();
		</script></body></html>`)
	})

	log.Printf("[INFO] Web Server di :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func forwardToAPI(baseURL, deviceID string, value float64) {
	url := fmt.Sprintf("%s?device_id=%s&value=%.2f", baseURL, deviceID, value)
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err == nil {
		resp.Body.Close()
	}
}
