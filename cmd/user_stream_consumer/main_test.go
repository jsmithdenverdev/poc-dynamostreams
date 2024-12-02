package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// mockDynamoDBClient implements dynamoDBClient interface for testing
type mockDynamoDBClient struct {
	batchWriteItemFunc func(ctx context.Context, params *dynamodb.BatchWriteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.BatchWriteItemOutput, error)
}

func (m *mockDynamoDBClient) BatchWriteItem(ctx context.Context, params *dynamodb.BatchWriteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.BatchWriteItemOutput, error) {
	return m.batchWriteItemFunc(ctx, params, optFns...)
}

// Test_handler verifies the Lambda handler's behavior for different DynamoDB stream events
func Test_handler(t *testing.T) {
	tests := []struct {
		name           string
		event          events.SQSEvent
		getenv         func(string) string
		expectedError  error
		expectedWrites int
		mockBatchWrite func(ctx context.Context, params *dynamodb.BatchWriteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.BatchWriteItemOutput, error)
	}{
		{
			name: "successful write with multiple organizations",
			event: events.SQSEvent{
				Records: []events.SQSMessage{
					{
						Body: `{
							"eventName": "INSERT",
							"dynamodb": {
								"NewImage": {
									"pk": {"S": "USER#123"},
									"organizations": {"L": [
										{"S": "org1"},
										{"S": "org2"}
									]}
								}
							}
						}`,
					},
				},
			},
			getenv: func(string) string { return "test-table" },
			mockBatchWrite: func(ctx context.Context, params *dynamodb.BatchWriteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.BatchWriteItemOutput, error) {
				if len(params.RequestItems["test-table"]) != 2 {
					t.Errorf("expected 2 write requests, got %d", len(params.RequestItems["test-table"]))
				}
				return &dynamodb.BatchWriteItemOutput{}, nil
			},
			expectedError: nil,
		},
		{
			name: "skip modify event",
			event: events.SQSEvent{
				Records: []events.SQSMessage{
					{
						Body: `{
							"eventName": "REMOVE",
							"dynamodb": {
								"NewImage": null
							}
						}`,
					},
				},
			},
			getenv: func(string) string { return "test-table" },
			mockBatchWrite: func(ctx context.Context, params *dynamodb.BatchWriteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.BatchWriteItemOutput, error) {
				t.Error("BatchWriteItem should not be called for REMOVE events")
				return nil, nil
			},
			expectedError: nil,
		},
		{
			name: "failed batch write",
			event: events.SQSEvent{
				Records: []events.SQSMessage{
					{
						Body: `{
								"eventName": "INSERT",
								"dynamodb": {
									"NewImage": {
										"pk": {"S": "USER#123"},
										"organizations": {"L": [
											{"S": "org1"},
											{"S": "org2"}
										]}
									}
								}
							}`,
					},
				},
			},
			getenv:        func(string) string { return "test-table" },
			expectedError: fmt.Errorf("failed to batch write organization memberships: simulated batch write error"),
			mockBatchWrite: func(ctx context.Context, params *dynamodb.BatchWriteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.BatchWriteItemOutput, error) {
				return nil, fmt.Errorf("simulated batch write error")
			},
		},
		{
			name: "delete user removes all memberships",
			event: events.SQSEvent{
				Records: []events.SQSMessage{
					{
						Body: `{
							"eventName": "REMOVE",
							"dynamodb": {
								"OldImage": {
									"pk": {"S": "USER#123"},
									"organizations": {"L": [
										{"S": "org1"},
										{"S": "org2"}
									]}
								}
							}
						}`,
					},
				},
			},
			getenv: func(string) string { return "test-table" },
			mockBatchWrite: func(ctx context.Context, params *dynamodb.BatchWriteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.BatchWriteItemOutput, error) {
				if len(params.RequestItems["test-table"]) != 2 {
					t.Errorf("expected 2 delete requests, got %d", len(params.RequestItems["test-table"]))
				}
				for _, req := range params.RequestItems["test-table"] {
					if req.DeleteRequest == nil {
						t.Error("expected DeleteRequest, got PutRequest")
					}
				}
				return &dynamodb.BatchWriteItemOutput{}, nil
			},
		},
		{
			name: "update user syncs memberships",
			event: events.SQSEvent{
				Records: []events.SQSMessage{
					{
						Body: `{
							"eventName": "MODIFY",
							"dynamodb": {
								"OldImage": {
									"pk": {"S": "USER#123"},
									"organizations": {"L": [
										{"S": "org1"},
										{"S": "org2"}
									]}
								},
								"NewImage": {
									"pk": {"S": "USER#123"},
									"organizations": {"L": [
										{"S": "org2"},
										{"S": "org3"}
									]}
								}
							}
						}`,
					},
				},
			},
			getenv: func(string) string { return "test-table" },
			mockBatchWrite: func(ctx context.Context, params *dynamodb.BatchWriteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.BatchWriteItemOutput, error) {
				if len(params.RequestItems["test-table"]) != 2 {
					t.Errorf("expected 2 requests, got %d", len(params.RequestItems["test-table"]))
				}

				var deleteCount, putCount int
				for _, req := range params.RequestItems["test-table"] {
					if req.DeleteRequest != nil {
						deleteCount++
					}
					if req.PutRequest != nil {
						putCount++
					}
				}
				if deleteCount != 1 || putCount != 1 {
					t.Errorf("expected 1 delete and 1 put, got %d deletes and %d puts", deleteCount, putCount)
				}
				return &dynamodb.BatchWriteItemOutput{}, nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
			mockClient := &mockDynamoDBClient{
				batchWriteItemFunc: tt.mockBatchWrite,
			}

			h := handler(logger, mockClient, tt.getenv)
			err := h(context.Background(), tt.event)

			if tt.expectedError == nil {
				if err != nil {
					t.Errorf("handler() unexpected error = %v", err)
				}
			} else if err == nil || err.Error() != tt.expectedError.Error() {
				t.Errorf("handler() error = %v, want %v", err, tt.expectedError)
			}
		})
	}
}

// Test_extractUserID verifies the user ID extraction from composite keys
func Test_extractUserID(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "valid composite key",
			input:    "USER#123",
			expected: "123",
		},
		{
			name:     "no separator",
			input:    "USER123",
			expected: "USER123",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractUserID(tt.input)
			if result != tt.expected {
				t.Errorf("extractUserID() = %v, want %v", result, tt.expected)
			}
		})
	}
}

// Test_createWriteRequests verifies the creation of DynamoDB write requests
func Test_createWriteRequests(t *testing.T) {
	tests := []struct {
		name     string
		userPK   string
		orgs     []string
		isDelete bool

		wantLen      int
		verifyResult func(t *testing.T, requests []types.WriteRequest)
	}{
		{
			name:     "create put requests",
			userPK:   "USER#123",
			orgs:     []string{"org1", "org2"},
			isDelete: false,
			wantLen:  2,
			verifyResult: func(t *testing.T, requests []types.WriteRequest) {
				for i, req := range requests {
					if req.PutRequest == nil {
						t.Errorf("request %d: expected PutRequest, got DeleteRequest", i)
					}
					item := req.PutRequest.Item
					pk := item["pk"].(*types.AttributeValueMemberS).Value
					sk := item["sk"].(*types.AttributeValueMemberS).Value
					expectedPK := fmt.Sprintf("ORGANIZATION#%s", []string{"org1", "org2"}[i])
					expectedSK := "MEMBERSHIP#123"
					if pk != expectedPK || sk != expectedSK {
						t.Errorf("request %d: got pk=%s, sk=%s, want pk=%s, sk=%s",
							i, pk, sk, expectedPK, expectedSK)
					}
				}
			},
		},
		{
			name:     "create delete requests",
			userPK:   "USER#456",
			orgs:     []string{"org3", "org4"},
			isDelete: true,
			wantLen:  2,
			verifyResult: func(t *testing.T, requests []types.WriteRequest) {
				for i, req := range requests {
					if req.DeleteRequest == nil {
						t.Errorf("request %d: expected DeleteRequest, got PutRequest", i)
					}
					key := req.DeleteRequest.Key
					pk := key["pk"].(*types.AttributeValueMemberS).Value
					sk := key["sk"].(*types.AttributeValueMemberS).Value
					expectedPK := fmt.Sprintf("ORGANIZATION#%s", []string{"org3", "org4"}[i])
					expectedSK := "MEMBERSHIP#456"
					if pk != expectedPK || sk != expectedSK {
						t.Errorf("request %d: got pk=%s, sk=%s, want pk=%s, sk=%s",
							i, pk, sk, expectedPK, expectedSK)
					}
				}
			},
		},
		{
			name:     "empty organizations list",
			userPK:   "USER#789",
			orgs:     []string{},
			isDelete: false,
			wantLen:  0,
			verifyResult: func(t *testing.T, requests []types.WriteRequest) {
				if len(requests) != 0 {
					t.Errorf("expected empty request list, got %d requests", len(requests))
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := createWriteRequests(tt.userPK, tt.orgs, tt.isDelete)
			if len(result) != tt.wantLen {
				t.Errorf("createWriteRequests() returned %d requests, want %d", len(result), tt.wantLen)
			}
			tt.verifyResult(t, result)
		})
	}
}
