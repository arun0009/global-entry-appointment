// main_test.go
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/kelseyhightower/envconfig"
	"github.com/stretchr/testify/assert"
	api "github.com/twilio/twilio-go/rest/api/v2010"
)

// MockTwilioClient is a mock implementation of the Twilio client interface.
type MockTwilioClient struct {
	// createMessageFunc holds a function that simulates the CreateMessage behavior.
	createMessageFuncMock func(params *api.CreateMessageParams) (*api.ApiV2010Message, error)

	// Store parameters passed to CreateMessage for assertion
	receivedCreateMessageParams *api.CreateMessageParams
}

func (m *MockTwilioClient) CreateMessage(params *api.CreateMessageParams) (*api.ApiV2010Message, error) {
	// Store parameters for assertion
	m.receivedCreateMessageParams = params

	// Call the mock function to simulate behavior.
	return m.createMessageFuncMock(params)
}

func TestHandler(t *testing.T) {

	t.Setenv("AWS_ACCOUNT_ID", "808475159191")
	t.Setenv("TWILIO_ACCOUNT_SID", "accountSid")
	t.Setenv("TWILIO_AUTH_TOKEN", "authToken")
	t.Setenv("LOCATIONID", "5002")
	t.Setenv("TWILIOFROM", "+18176152689")
	t.Setenv("TWILIOTO", "+14182005000")

	// Mocking HTTP server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, fmt.Sprintf("%s?%s", r.URL.Path, r.URL.RawQuery), "/schedulerapi/slots?orderBy=soonest&limit=1&locationId=5002&minimum=1")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"locationId":5002,"startTimestamp":"2024-04-01 02:00:00","endTimestamp":"2024-04-01 02:30:00","active":true,"duration":30,"remoteInd":false}]`))
	}))
	defer server.Close()

	url := server.URL + "/schedulerapi/slots?orderBy=soonest&limit=1&locationId=%s&minimum=1"

	var config Config
	err := envconfig.Process("", &config)
	if err != nil {
		panic(err)
	}

	// Mock Twilio client
	mockTwilioClient := &MockTwilioClient{createMessageFuncMock: func(params *api.CreateMessageParams) (*api.ApiV2010Message, error) {
		return &api.ApiV2010Message{}, nil
	},
	}

	handler := &LambdaHandler{url, config, mockTwilioClient}
	resp, err := handler.HandleRequest(context.Background(), events.APIGatewayProxyRequest{})
	assert.NoError(t, err)
	assert.Equal(t, resp.Body, `Success!`)
	// Assert the message parameters passed to Twilio
	assert.NotNil(t, mockTwilioClient.receivedCreateMessageParams)
	assert.Equal(t, "There is a global entry appointment open at 5002", *mockTwilioClient.receivedCreateMessageParams.Body)
	assert.Equal(t, "+18176152689", *mockTwilioClient.receivedCreateMessageParams.From)
	assert.Equal(t, "+14182005000", *mockTwilioClient.receivedCreateMessageParams.To)

}
