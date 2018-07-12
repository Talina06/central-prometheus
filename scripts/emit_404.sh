max=15
for i in `seq 2 $max`
do
	sleep 3
	/bin/bash -c "curl http://localhost:8080/err"
done

