package main

import (
	"fmt"
	"time"

	"github.com/multi-agents-etf-trade-strategy/datasource"
)

func main() {
	ds := datasource.NewEastMoneyDataSource()
	asOf, _ := time.ParseInLocation("2006-01-02", "2026-05-26", time.Local)

	fmt.Println("=== days=22 ===")
	k, err := ds.GetKLineAsOf("513520", 22, asOf)
	fmt.Printf("err=%v len=%d\n", err, len(k))
	if len(k) > 0 {
		first := k[0]
		last := k[len(k)-1]
		fmt.Printf("first: date=%s O=%.3f C=%.3f H=%.3f L=%.3f V=%.0f\n", first.Date.Format("2006-01-02"), first.Open, first.Close, first.High, first.Low, first.Volume)
		fmt.Printf("last : date=%s O=%.3f C=%.3f H=%.3f L=%.3f V=%.0f\n", last.Date.Format("2006-01-02"), last.Open, last.Close, last.High, last.Low, last.Volume)
	}

	fmt.Println()
	fmt.Println("=== days=60 (Screener 用) ===")
	k60, err := ds.GetKLineAsOf("513520", 60, asOf)
	fmt.Printf("err=%v len=%d\n", err, len(k60))
	if len(k60) > 0 {
		last := k60[len(k60)-1]
		fmt.Printf("last : date=%s O=%.3f C=%.3f H=%.3f L=%.3f V=%.0f\n", last.Date.Format("2006-01-02"), last.Open, last.Close, last.High, last.Low, last.Volume)
	}
}
