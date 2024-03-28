// main.go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/kelseyhightower/envconfig"
	"github.com/twilio/twilio-go"
	api "github.com/twilio/twilio-go/rest/api/v2010"
)

type (
	//Config configuration set via env variables
	Config struct {
		LocationID string
		TwilioFrom string
		TwilioTo   string
	}

	//Appointment json returned by global entry api response
	Appointment struct {
		LocationID     int    `json:"locationId"`
		StartTimestamp string `json:"startTimestamp"`
		EndTimestamp   string `json:"endTimestamp"`
		Active         bool   `json:"active"`
		Duration       int    `json:"duration"`
		RemoteInt      bool   `json:"remoteInd"`
	}

	//TwilioClient interface for sending message makes testing and changing client easier
	TwilioClient interface {
		CreateMessage(params *api.CreateMessageParams) (*api.ApiV2010Message, error)
	}

	//LambdaHandler inject required values from main and test
	LambdaHandler struct {
		URL    string
		Config Config
		Client TwilioClient
	}
)

// NewLambdaHandler creates new LambdaHandler
func NewLambdaHandler(url string, config Config) *LambdaHandler {
	return &LambdaHandler{
		URL:    url,
		Config: config,
		Client: twilio.NewRestClient().Api,
	}
}

// HandleRequest handles request from lambda invocation.
func (h *LambdaHandler) HandleRequest(ctx context.Context, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	url := fmt.Sprintf(h.URL, h.Config.LocationID)
	response, err := http.Get(url)
	if err != nil {
		return events.APIGatewayProxyResponse{StatusCode: 500}, fmt.Errorf("failed to get appointment slots: %w", err)
	}
	defer response.Body.Close()

	responseData, err := io.ReadAll(response.Body)
	if err != nil {
		return events.APIGatewayProxyResponse{StatusCode: 500}, fmt.Errorf("failed to read response body: %w", err)
	}

	var appointments []Appointment
	err = json.Unmarshal(responseData, &appointments)
	if err != nil {
		return events.APIGatewayProxyResponse{StatusCode: 500}, fmt.Errorf("failed to unmarshal response data: %w", err)
	}

	if len(appointments) > 0 {
		params := &api.CreateMessageParams{}
		params.SetBody("There is a global entry appointment open at " + strconv.Itoa(appointments[0].LocationID))
		params.SetFrom(h.Config.TwilioFrom)
		params.SetTo(h.Config.TwilioTo)
		_, err := h.Client.CreateMessage(params)
		if err != nil {
			return events.APIGatewayProxyResponse{StatusCode: 500}, fmt.Errorf("failed to send message: %w", err)
		}
	}
	return events.APIGatewayProxyResponse{
		StatusCode: 200,
		Body:       "Success!",
	}, nil
}

func main() {
	var config Config
	err := envconfig.Process("", &config)
	if err != nil {
		panic(err)
	}

	url := "https://ttp.cbp.dhs.gov/schedulerapi/slots?orderBy=soonest&limit=1&locationId=%s&minimum=1"
	handler := NewLambdaHandler(url, config)
	lambda.Start(handler.HandleRequest)
}
