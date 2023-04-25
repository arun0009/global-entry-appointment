// main.go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"strconv"
	
	"github.com/kelseyhightower/envconfig"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"

	"github.com/twilio/twilio-go"
	api "github.com/twilio/twilio-go/rest/api/v2010"
)

type (
	Config struct {
		LocationID string
		TwilioFrom string
		TwilioTo   string
	}

	Appointment struct {
		LocationID     int    `json:"locationId"`
		StartTimestamp string `json:"startTimestamp"`
		EndTimestamp   string `json:"endTimestamp"`
		Active         bool   `json:"active"`
		Duration       int    `json:"duration"`
		RemoteInt      bool   `json:"remoteInd"`
	}
)

func Handler(ctx context.Context, request events.APIGatewayProxyRequest) {
	var config Config
	err := envconfig.Process("", &config)
	url := fmt.Sprintf("https://ttp.cbp.dhs.gov/schedulerapi/slots?orderBy=soonest&limit=1&locationId=%s&minimum=1", config.LocationID)
	response, err := http.Get(url)

	if err != nil {
		log.Fatal(err)
	}
	responseData, err := ioutil.ReadAll(response.Body)
	if err != nil {
		log.Fatal(err)
	}

	var appointment []Appointment

	err = json.Unmarshal(responseData, &appointment)
	if err != nil {
		log.Fatal(err)
	}

	if len(appointment) > 0 {
		log.Println(string(responseData))
		client := twilio.NewRestClient()
		params := &api.CreateMessageParams{}
		params.SetBody("There is a global entry appointment open at " + strconv.Itoa(appointment[0].LocationID))
		params.SetFrom(config.TwilioFrom)
		params.SetTo(config.TwilioTo)
		_, err := client.Api.CreateMessage(params)
		if err != nil {
			log.Println(err.Error())
		}
	}
}

func main() {
	// Make the handler available for Remote Procedure Call by AWS Lambda
	lambda.Start(Handler)
}
