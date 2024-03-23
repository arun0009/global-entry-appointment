export GOOS=linux
export GOARCH=amd64
export CGO_ENABLED=0
.DEFAULT_GOAL := deploy

deploy:
	go build -o bootstrap
	zip -r bootstrap.zip bootstrap
	aws iam create-role --role-name lambda-basic-execution --assume-role-policy-document file://lambda-trust-policy.json	
	aws iam attach-role-policy --role-name lambda-basic-execution --policy-arn arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole
	sleep 5
	aws lambda create-function --function-name "GlobalEntry" --handler globalentry --zip-file fileb://bootstrap.zip \
	--region="us-east-1" \
	--environment '{"Variables":{"TWILIO_ACCOUNT_SID":"${TWILIO_ACCOUNT_SID}","TWILIO_AUTH_TOKEN":"${TWILIO_AUTH_TOKEN}",\
				   "LOCATIONID":"${LOCATIONID}","TWILIOFROM":"${TWILIOFROM}","TWILIOTO":"${TWILIOTO}"}}' \
	--role "arn:aws:iam::${AWS_ACCOUNT_ID}:role/lambda-basic-execution" \
	--runtime provided.al2023
	aws events put-rule \
	--name GlobalEntryCron \
	--region="us-east-1" \
	--state 'ENABLED' \
	--schedule-expression 'rate(1 minute)'
	aws lambda add-permission \
	--region="us-east-1" \
	--function-name GlobalEntry \
	--statement-id GlobalEntryEvent \
	--action 'lambda:InvokeFunction' \
	--principal 'events.amazonaws.com' \
	--source-arn "arn:aws:events:us-east-1:${AWS_ACCOUNT_ID}:rule/GlobalEntryCron"
	aws events put-targets --rule GlobalEntryCron --targets \
	--region="us-east-1" \
	'{"Id":"1","Arn":"arn:aws:lambda:us-east-1:${AWS_ACCOUNT_ID}:function:GlobalEntry"}'

redeploy:
	go build -o bootstrap
	zip -r bootstrap.zip bootstrap
	aws lambda update-function-code --function-name "GlobalEntry" --zip-file fileb://bootstrap.zip --region="us-east-1"
