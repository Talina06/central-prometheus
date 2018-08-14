max=500
for i in `seq 2 $max`
do
	sleep 2
	/bin/bash -c "curl http://localhost:8081"
done
