package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type User struct {
	PK            string   `json:"pk"`
	SK            string   `json:"sk"`
	Email         string   `json:"email"`
	Status        string   `json:"status"`
	Organizations []string `json:"organizations"`
}

type OrganizationMembership struct {
	PK string `dynamodbav:"pk"`
	SK string `dynamodbav:"sk"`
}

func extractUserID(compositeKey string) string {
	parts := strings.Split(compositeKey, "#")
	if len(parts) < 2 {
		return compositeKey // fallback to original if not in expected format
	}
	return parts[1]
}

func main() {
	if err := run(context.Background(), os.Stdout, os.Getenv); err != nil {
		fmt.Fprintf(os.Stderr, "failed run: %s", err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context, stdout io.Writer, getenv func(string) string) error {
	logger := slog.New(slog.NewJSONHandler(stdout, nil))

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	client := dynamodb.NewFromConfig(cfg)
	handler := handler(logger, client, getenv)
	lambda.Start(handler)
	return nil
}

func handler(logger *slog.Logger, client *dynamodb.Client, getenv func(string) string) func(ctx context.Context, event events.SQSEvent) error {
	tableName := getenv("TABLE_NAME")
	if tableName == "" {
		tableName = "poc-organizations"
	}

	return func(ctx context.Context, event events.SQSEvent) error {
		logger.InfoContext(ctx, "processing sqs event", slog.Int("records", len(event.Records)))

		for _, record := range event.Records {
			var der events.DynamoDBEventRecord
			if err := json.Unmarshal([]byte(record.Body), &der); err != nil {
				logger.ErrorContext(ctx, "failed to unmarshal dynamo event",
					slog.String("error", err.Error()),
					slog.String("body", record.Body))
				return fmt.Errorf("failed to unmarshal DynamoDB event record: %w", err)
			}

			// Skip if not an INSERT or MODIFY event
			if der.EventName != string(events.DynamoDBOperationTypeInsert) &&
				der.EventName != string(events.DynamoDBOperationTypeModify) {
				logger.InfoContext(ctx, "skipping event", slog.String("eventName", der.EventName))
				continue
			}

			// Skip if no new image
			if der.Change.NewImage == nil {
				logger.InfoContext(ctx, "skipping event with no new image")
				continue
			}

			var user User
			user.PK = der.Change.NewImage["pk"].String()
			if orgs, ok := der.Change.NewImage["organizations"]; ok {
				for _, org := range orgs.List() {
					user.Organizations = append(user.Organizations, org.String())
				}
			}

			logger.InfoContext(ctx, "processing user organizations",
				slog.String("userPK", user.PK),
				slog.Int("organizationCount", len(user.Organizations)))

			writeRequests := make([]types.WriteRequest, 0, len(user.Organizations))
			for _, orgID := range user.Organizations {
				membership := OrganizationMembership{
					PK: fmt.Sprintf("ORGANIZATION#%s", orgID),
					SK: fmt.Sprintf("MEMBERSHIP#%s", extractUserID(user.PK)),
				}
				item, err := attributevalue.MarshalMap(membership)
				if err != nil {
					logger.ErrorContext(ctx, "failed to marshal membership",
						slog.String("error", err.Error()),
						slog.Any("membership", membership))
					return fmt.Errorf("failed to marshal OrganizationMembership to DynamoDB item: %w", err)
				}
				writeRequests = append(writeRequests, types.WriteRequest{
					PutRequest: &types.PutRequest{Item: item},
				})
			}

			input := &dynamodb.BatchWriteItemInput{
				RequestItems: map[string][]types.WriteRequest{
					tableName: writeRequests,
				},
			}

			logger.InfoContext(ctx, "writing organization memberships",
				slog.String("table", tableName),
				slog.Int("requestCount", len(writeRequests)),
				slog.Any("input", input))

			if _, err := client.BatchWriteItem(ctx, input); err != nil {
				logger.ErrorContext(ctx, "failed to batch write memberships",
					slog.String("error", err.Error()),
					slog.String("table", tableName),
					slog.Int("requestCount", len(writeRequests)))
				return fmt.Errorf("failed to batch write organization memberships: %w", err)
			}
		}

		return nil
	}
}
