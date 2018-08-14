FROM ubuntu
COPY adapter adapter
ENTRYPOINT ["./adapter", "-influxdb-url=http://influxdb:8086", "-influxdb.database=prometheus", "-influxdb.retention-policy=autogen"]
