package main

import (
	"encoding/json"
	"io"
	"os"

	awscdk "github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsevents"
	"github.com/aws/aws-cdk-go/awscdk/v2/awseventstargets"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	"github.com/aws/aws-cdk-go/awscdk/v2/awslambda"
	constructs "github.com/aws/constructs-go/constructs/v10"
	jsii "github.com/aws/jsii-runtime-go"
)

const (
	StackName    = "GlobalEntryStack"
	FunctionName = "globalentry"
	MemorySize   = 128
	MaxDuration  = 60
	CodePath     = ".bin/"
	Handler      = "main.Handler"
	ScheduleRate = 15
	EnvFilePath  = "env.json"
)

type LambdaCdkStackProps struct {
	awscdk.StackProps
}

type Environment struct {
	Parameters map[string]*string `json:"Parameters"`
	AWS        map[string]*string `json:'"AWS"`
}

func LoadEnvironmentVariables(filePath string) (map[string]*string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}

	var env Environment

	err = json.Unmarshal(data, &env)
	if err != nil {
		return nil, err
	}

	return env.Parameters, nil
}

func NewLambdaCdkStack(scope constructs.Construct, id string, props *LambdaCdkStackProps) awscdk.Stack {
	stack := awscdk.NewStack(scope, &id, &props.StackProps)

	// Load environment variables from JSON file
	envVars, err := LoadEnvironmentVariables(EnvFilePath)
	if err != nil {
		panic(err)
	}

	// Define Lambda function
	globalEntryFn := awslambda.NewFunction(stack, jsii.String(FunctionName), &awslambda.FunctionProps{
		FunctionName: jsii.String(*stack.StackName() + "-" + FunctionName),
		Runtime:      awslambda.Runtime_PROVIDED_AL2023(),
		MemorySize:   jsii.Number(MemorySize),
		Timeout:      awscdk.Duration_Seconds(jsii.Number(MaxDuration)),
		Code:         awslambda.AssetCode_FromAsset(jsii.String(CodePath), nil),
		Handler:      jsii.String(Handler),
		Environment:  &envVars,
	})

	// Define CloudWatch event rule
	rule := awsevents.NewRule(stack, jsii.String("GlobalEntryScheduledRule"), &awsevents.RuleProps{
		Schedule: awsevents.Schedule_Rate(awscdk.Duration_Minutes(jsii.Number(ScheduleRate))),
	})

	// Get the ARN of the CloudWatch Events rule
	ruleArn := rule.RuleArn()

	// Add permission for the event rule to invoke the Lambda function
	globalEntryFn.AddPermission(jsii.String("AllowEventRule"),
		&awslambda.Permission{
			Action:    jsii.String("lambda:InvokeFunction"),
			Principal: awsiam.NewServicePrincipal(jsii.String("events.amazonaws.com"), &awsiam.ServicePrincipalOpts{}),
			SourceArn: ruleArn,
		},
	)

	// Add Lambda function as a target for the rule
	rule.AddTarget(awseventstargets.NewLambdaFunction(globalEntryFn, &awseventstargets.LambdaFunctionProps{}))

	return stack
}

func main() {
	app := awscdk.NewApp(nil)
	LoadEnvironmentVariables("env.json")

	NewLambdaCdkStack(app, StackName, &LambdaCdkStackProps{
		awscdk.StackProps{
			Env: &awscdk.Environment{
				Account: jsii.String(os.Getenv("AWS_ACCOUNT")),
				Region:  jsii.String(os.Getenv("AWS_REGION")),
			},
		},
	})

	app.Synth(nil)
}
