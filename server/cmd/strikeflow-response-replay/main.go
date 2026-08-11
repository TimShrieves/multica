package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/integrations/strikeflowresponse"
)

func main() {
	commandID := flag.String("command-id", "", "exact canary command UUID")
	eventID := flag.String("event-id", "", "exact delivered comment event UUID")
	payloadSHA := flag.String("payload-sha256", "", "immutable StrikeFlow payload SHA256")
	recordedAt := flag.String("recorded-at", "", "immutable StrikeFlow recorded_at timestamp")
	flag.Parse()
	if flag.NArg() != 0 || *commandID == "" || *eventID == "" || *payloadSHA == "" || *recordedAt == "" {
		fmt.Fprintln(os.Stderr, "all exact replay flags are required")
		os.Exit(64)
	}
	config, err := strikeflowresponse.ConfigFromEnv()
	if err != nil {
		fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		fatal(err)
	}
	defer pool.Close()
	result, err := strikeflowresponse.ReplayDeliveredComment(ctx, pool, config, *commandID, *eventID, *payloadSHA, *recordedAt)
	if err != nil {
		fatal(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
