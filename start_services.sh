if [ $# -eq 0 ]
  then
    echo "Please provide the Pagerduty Prometheus Integration key."
    exit 1
fi
cd scripts/ && bash pagerduty_service_key.sh $1 && cd ../
cd deploy/ && sudo docker-compose up && cd ../

