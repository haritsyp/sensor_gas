package main

import (
	"encoding/json"
	"log"
	"net/http"
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

func main() {
	handler := modbus.NewTCPClientHandler("192.168.31.109:502")
	handler.Timeout = 5 * time.Second
	handler.SlaveId = 1

	err := handler.Connect()
	if err != nil {
		log.Fatalf("[FATAL] %v", err)
	}
	defer handler.Close()
	client := modbus.NewClient(handler)

	log.Printf("[INFO] Memulai DEEP SCAN (MW0-100) untuk mencari nilai 20000...")

	go func() {
		ticker := time.NewTicker(2 * time.Second)
		for range ticker.C {
			for block := 0; block < 5; block++ {
				startAddr := uint16(block * 20)
				regs, err := client.ReadHoldingRegisters(startAddr, 20)
				if err == nil {
					for i := 0; i < 20; i++ {
						val := uint16(regs[i*2])<<8 | uint16(regs[i*2+1])
						if val > 100 { 
							log.Printf("[FOUND] Alamat MW%d: %d", int(startAddr)+i, val)
						}
					}
				}
				time.Sleep(100 * time.Millisecond)
			}
			
			status, err := client.ReadHoldingRegisters(0, 3)
			if err == nil {
				mu.Lock()
				dataStore.Gas1 = float64(uint16(status[0])<<8 | uint16(status[1]))
				dataStore.Gas2 = float64(uint16(status[2])<<8 | uint16(status[3]))
				dataStore.Gas3 = float64(uint16(status[4])<<8 | uint16(status[5]))
				dataStore.Fault1 = (dataStore.Gas1 == 0)
				dataStore.Fault2 = (dataStore.Gas2 == 0)
				dataStore.Fault3 = (dataStore.Gas3 == 0)
				dataStore.LastUpdate = time.Now().Format("15:04:05")
				mu.Unlock()
			}
		}
	}()

	http.HandleFunc("/api/data", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		json.NewEncoder(w).Encode(dataStore)
		mu.Unlock()
	})

	log.Fatal(http.ListenAndServe(":8080", nil))
}
