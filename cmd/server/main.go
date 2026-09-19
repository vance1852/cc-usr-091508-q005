package main

import (
	"log"
	"os"

	"airspace/internal/app"
	"airspace/internal/store"
)

func main() {
	dbPath := os.Getenv("AIRSPACE_DB")
	if dbPath == "" {
		dbPath = "airspace.db"
	}
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}

	db, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if err := store.Migrate(db); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	seeded, err := app.SeedIfEmpty(db)
	if err != nil {
		log.Fatalf("seed: %v", err)
	}
	if seeded {
		log.Print("空库已写入演示数据（账号见 README，令牌形如 tok-atc / tok-pilot-a1）")
	}

	e := app.NewServer(db)
	log.Printf("临时空域授权服务监听 %s", addr)
	log.Fatal(e.Start(addr))
}
