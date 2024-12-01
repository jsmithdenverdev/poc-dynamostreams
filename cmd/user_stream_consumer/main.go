package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
)

func main() {
	if err := run(os.Stdout, os.Getenv); err != nil {
		fmt.Fprintf(os.Stderr, "failed run: %s", err.Error())
		os.Exit(1)
	}
}

func run(stdout io.Writer, getenv func(string) string) error {
	logger := slog.New(slog.NewJSONHandler(stdout, nil))
	handler := handler(logger)
	lambda.Start(handler)
	return nil
}

func handler(logger *slog.Logger) func(ctx context.Context, event events.SQSEvent) error {
	return func(ctx context.Context, event events.SQSEvent) error {
		var records []events.DynamoDBEvent

		for _, record := range event.Records {
			var dynamoevent events.DynamoDBEvent
			_ = json.Unmarshal([]byte(record.Body), &dynamoevent)
			records = append(records, dynamoevent)
		}

		logger.InfoContext(
			ctx,
			"user_stream_consumer",
			slog.Any("event", event),
			slog.Any("records", records))

		return nil
	}
}
