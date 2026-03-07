BUILD_TO_DIR := .bin
GO_LINUX := GOOS=linux GOARCH=amd64 CGO_ENABLED=0
# Use arm64 for deploy: same price, better perf; use amd64 for local SAM if needed
GO_LINUX_ARM64 := GOOS=linux GOARCH=arm64 CGO_ENABLED=0
LDFLAGS := -ldflags="-s -w"

export AWS_ACCOUNT=889453232531
export AWS_REGION=us-east-1
export JSII_SILENCE_WARNING_UNTESTED_NODE_VERSION=1

.PHONY: help clean develop-clean develop build invoke aws-login update-creds deploy destroy

help:
	@echo "Global Entry Appointment - Make targets"
	@echo ""
	@echo "  make              same as make help"
	@echo "  make help         show this help"
	@echo "  make develop      build Lambda for local (amd64); output in .bin/"
	@echo "  make build        build Lambda for production (arm64, stripped); used by deploy"
	@echo "  make invoke       run SAM local API (depends on develop); http://127.0.0.1:9070"
	@echo "  make clean        remove .aws-sam"
	@echo "  make aws-login    AWS SSO login for deploy"
	@echo "  make update-creds run and source to assume AdminRole for CLI"
	@echo "  make deploy       build (arm64) and cdk deploy"
	@echo "  make destroy      cdk destroy"
	@echo ""

clean:
	rm -rf .aws-sam

develop-clean:
	rm -rf $(BUILD_TO_DIR)
	mkdir -p $(BUILD_TO_DIR)

develop: develop-clean
	go fmt ./...
	$(GO_LINUX) go build $(LDFLAGS) -o $(BUILD_TO_DIR)/bootstrap ./lambda/main.go

# Production build: ARM64 + stripped binary for lower cost and faster cold start
build: develop-clean
	go fmt ./...
	$(GO_LINUX_ARM64) go build $(LDFLAGS) -o $(BUILD_TO_DIR)/bootstrap ./lambda/main.go

invoke: develop
	sam local start-api --env-vars env.json --template globalentry.yaml --region ${AWS_REGION} --port 9070 --docker-network host --invoke-image amazon/aws-sam-cli-emulation-image-go1.x --skip-pull-image --log-file /dev/stdout

aws-login:
	aws sso login --profile ${AWS_ACCOUNT}_AdministratorAccess

#run output of this command so environment variables are set.
update-creds:	
	export $(shell printf "AWS_ACCESS_KEY_ID=%s AWS_SECRET_ACCESS_KEY=%s AWS_SESSION_TOKEN=%s" \
	$(shell aws sts assume-role \
	--profile ${AWS_ACCOUNT}_AdministratorAccess \
	--role-arn arn:aws:iam::${AWS_ACCOUNT}:role/AdminRole \
	--role-session-name AWSCLI-Session \
	--query "Credentials.[AccessKeyId,SecretAccessKey,SessionToken]" \
	--output text))

# Deploy uses production build (ARM64 + stripped) for lower cost
deploy: build
	cdk deploy

destroy:
	cdk destroy	