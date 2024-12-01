package main

import (
	"context"
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
		logger.InfoContext(ctx, "handler called", slog.Any("event", event))
		return nil
	}
}
