# Global Entry Appointment Scanner
This Repo runs a AWS Lambda that scans for appoinment (every 1 min, change in `Makefile` if you want a different schedule).

### Prerequisites
[AWS CLI](https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html)
[Free Twilio Account](https://www.twilio.com/try-twilio)

### ENV Variables

Set below environment variables
 
 ```bash
export AWS_ACCOUNT_ID=YOUR_AWS_ACCOUNT_ID
export TWILIO_ACCOUNT_SID=YOUR_ACCOUNT_SID
export TWILIO_AUTH_TOKEN=YOUR_TOKEN
export LOCATIONID=YOUR_APPOINTMENT_LOCATION_ID
export TWILIOFROM=YOUR_TWILIO_FROM_NUMBER
export TWILIOTO=YOUR_NUMBER_WHERE_YOU_WANT_NOTIFICATIONS
```

### Deployment

```bash 
make deploy
make redeploy (updates to Lambda are redeployed)
```

