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

// Package main provides a Lambda function that processes DynamoDB stream events from SQS
// and maintains organization memberships in a separate DynamoDB table.

// user represents a user record from the users table with their organization memberships
// and metadata.
type user struct {
	PK            string   `json:"pk"`            // Primary key in format "USER#<id>"
	SK            string   `json:"sk"`            // Sort key
	Email         string   `json:"email"`         // User's email address
	Status        string   `json:"status"`        // User's status
	Organizations []string `json:"organizations"` // List of organization IDs
}

// organizationMembership represents a membership record in the organizations table
// linking an organization to a user.
type organizationMembership struct {
	PK string `dynamodbav:"pk"` // Primary key in format "ORGANIZATION#<id>"
	SK string `dynamodbav:"sk"` // Sort key in format "MEMBERSHIP#<user_id>"
}

// dynamoDBClient defines the interface for DynamoDB operations required by the Lambda function.
// This interface helps with testing by allowing mock implementations.
type dynamoDBClient interface {
	BatchWriteItem(ctx context.Context, params *dynamodb.BatchWriteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.BatchWriteItemOutput, error)
}

// extractUserID extracts the ID portion from a composite key (e.g., "USER#123" -> "123").
// If the key doesn't contain a separator, returns the original string.
func extractUserID(compositeKey string) string {
	parts := strings.Split(compositeKey, "#")
	if len(parts) < 2 {
		return compositeKey // fallback to original if not in expected format
	}
	return parts[1]
}

// main is the entry point for the Lambda function. It initializes the runtime
// with a background context and standard output/environment configuration.
func main() {
	if err := run(context.Background(), os.Stdout, os.Getenv); err != nil {
		fmt.Fprintf(os.Stderr, "failed run: %s", err.Error())
		os.Exit(1)
	}
}

// run initializes the Lambda handler with AWS configuration and starts the runtime.
// It accepts a context for AWS operations, an io.Writer for logging output, and
// a function to retrieve environment variables.
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

// handler creates a Lambda handler that processes DynamoDB stream events from SQS.
// For each user record change, it creates or updates corresponding organization membership
// records in a target DynamoDB table. It handles INSERT, MODIFY, and REMOVE events,
// maintaining consistency between user organizations and membership records.
func handler(logger *slog.Logger, client dynamoDBClient, getenv func(string) string) func(ctx context.Context, event events.SQSEvent) error {
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

			var writeRequests []types.WriteRequest

			switch der.EventName {
			case string(events.DynamoDBOperationTypeRemove):
				if der.Change.OldImage == nil {
					continue
				}
				var oldUser user
				oldUser.PK = der.Change.OldImage["pk"].String()
				if orgs, ok := der.Change.OldImage["organizations"]; ok {
					for _, org := range orgs.List() {
						oldUser.Organizations = append(oldUser.Organizations, org.String())
					}
				}
				writeRequests = createWriteRequests(oldUser.PK, oldUser.Organizations, true)

			case string(events.DynamoDBOperationTypeModify):
				if der.Change.NewImage == nil {
					continue
				}

				// Get old and new organizations
				var oldOrgs, newOrgs []string
				if der.Change.OldImage != nil {
					if orgs, ok := der.Change.OldImage["organizations"]; ok {
						for _, org := range orgs.List() {
							oldOrgs = append(oldOrgs, org.String())
						}
					}
				}

				userPK := der.Change.NewImage["pk"].String()
				if orgs, ok := der.Change.NewImage["organizations"]; ok {
					for _, org := range orgs.List() {
						newOrgs = append(newOrgs, org.String())
					}
				}

				// Find organizations to remove and add
				toRemove := make([]string, 0)
				for _, org := range oldOrgs {
					found := false
					for _, newOrg := range newOrgs {
						if org == newOrg {
							found = true
							break
						}
					}
					if !found {
						toRemove = append(toRemove, org)
					}
				}

				toAdd := make([]string, 0)
				for _, org := range newOrgs {
					found := false
					for _, oldOrg := range oldOrgs {
						if org == oldOrg {
							found = true
							break
						}
					}
					if !found {
						toAdd = append(toAdd, org)
					}
				}

				// Create write requests for removals and additions
				writeRequests = append(writeRequests, createWriteRequests(userPK, toRemove, true)...)
				writeRequests = append(writeRequests, createWriteRequests(userPK, toAdd, false)...)

			case string(events.DynamoDBOperationTypeInsert):
				if der.Change.NewImage == nil {
					continue
				}
				var user user
				user.PK = der.Change.NewImage["pk"].String()
				if orgs, ok := der.Change.NewImage["organizations"]; ok {
					for _, org := range orgs.List() {
						user.Organizations = append(user.Organizations, org.String())
					}
				}
				writeRequests = createWriteRequests(user.PK, user.Organizations, false)
			}

			if len(writeRequests) == 0 {
				continue
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

// createWriteRequests creates a slice of DynamoDB WriteRequests for the given user and organizations.
// For each organization, it creates either a PutRequest or DeleteRequest based on the isDelete flag.
// The requests are used to maintain organization membership records in the target table.
func createWriteRequests(userPK string, organizations []string, isDelete bool) []types.WriteRequest {
	requests := make([]types.WriteRequest, 0, len(organizations))
	for _, orgID := range organizations {
		if isDelete {
			requests = append(requests, types.WriteRequest{
				DeleteRequest: &types.DeleteRequest{
					Key: map[string]types.AttributeValue{
						"pk": &types.AttributeValueMemberS{Value: fmt.Sprintf("ORGANIZATION#%s", orgID)},
						"sk": &types.AttributeValueMemberS{Value: fmt.Sprintf("MEMBERSHIP#%s", extractUserID(userPK))},
					},
				},
			})
			continue
		}

		membership := organizationMembership{
			PK: fmt.Sprintf("ORGANIZATION#%s", orgID),
			SK: fmt.Sprintf("MEMBERSHIP#%s", extractUserID(userPK)),
		}
		item, err := attributevalue.MarshalMap(membership)
		if err != nil {
			continue // skip invalid items
		}
		requests = append(requests, types.WriteRequest{
			PutRequest: &types.PutRequest{Item: item},
		})
	}
	return requests
}
