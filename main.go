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

// GasData untuk response API & Dashboard
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

	// Ambil Slave ID dari ENV, default ke 1 jika tidak ada
	sID := 1
	if envID, err := strconv.Atoi(os.Getenv("SLAVE_ID")); err == nil && envID != 0 {
		sID = envID
	}

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
		log.Fatalf("[FATAL] Gagal koneksi: %v", err)
	}
	defer handler.Close()
	client := modbus.NewClient(handler)

	log.Printf("[INFO] Monitoring Aktif | PLC: %s | Slave ID: %d", config.ModbusAddr, config.SlaveID)

	go func() {
		ticker := time.NewTicker(config.PollingInterval)
		for range ticker.C {
			// Membaca MW0 - MW9
			regs, err := client.ReadHoldingRegisters(0, 3)
			if err != nil {
				log.Printf("[ERROR] Modbus Read: %v", err)
				continue
			}

			parseVal := func(b []byte, off int) (float64, bool) {
				raw := uint16(b[off])<<8 | uint16(b[off+1])
				// 0 = Fault (Kabel Putus)
				if raw == 0 {
					return 0, true
				}
				// > 1000 = Analog Raw
				if raw > 1000 {
					val := (float64(raw) - 4000) / 160.0
					if val < 0 {
						val = 0
					}
					if val > 100 {
						val = 100
					}
					return val, false
				}
				// 1 = OK (Status saja)
				return float64(raw), false
			}

			mu.Lock()
			dataStore.Gas1, dataStore.Fault1 = parseVal(regs, 0)
			dataStore.Gas2, dataStore.Fault2 = parseVal(regs, 2)
			dataStore.Gas3, dataStore.Fault3 = parseVal(regs, 4)
			dataStore.LastUpdate = time.Now().Format("15:04:05")
			currentData := dataStore
			mu.Unlock()

			// Forward ke API
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
					log.Printf("[LOG] ID %s: KABEL PUTUS", dID)
				} else {
					log.Printf("[LOG] ID %s: %.2f", dID, val)
				}
				go forwardToAPI(config.APIBaseURL, dID, sendVal)
			}
		}
	}()

	// Handlers
	http.HandleFunc("/api/data", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		json.NewEncoder(w).Encode(dataStore)
		mu.Unlock()
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<html><head><title>Gas Monitoring</title><style>body{font-family:sans-serif;background:#1a1a1a;color:white;text-align:center}.card{background:#2d2d2d;padding:20px;margin:10px;display:inline-block;border-radius:10px;width:200px;border-top:5px solid #4CAF50}.card.fault{border-top-color:#f44336}h1{font-size:3em}</style></head>
		<body><h1>Gas Monitoring</h1><div id="d"></div>
		<script>
			async function u(){
				try {
					const res=await fetch('/api/data');const d=await res.json();
					const c=(n,v,f)=>`+"`"+`<div class="card ${f?'fault':''}"><h3>Sensor ${n}</h3><h1>${f?'ERR':v}</h1><p style="color:${f?'#f44336':'#4CAF50'}">${f?'KABEL PUTUS':'OK'}</p></div>`+"`"+`;
					document.getElementById('d').innerHTML=c(1,d.gas1,d.fault1)+c(2,d.gas2,d.fault2)+c(3,d.gas3,d.fault3);
				} catch(e){}
			}setInterval(u,1000);u();
		</script></body></html>`)
	})

	log.Printf("[INFO] Dashboard: http://localhost:8080")
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
