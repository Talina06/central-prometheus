#!/bin/sh
/bin/sed -i "s/PAGERDUTY_KEY/$1/g" ../alertmanager/config.yml
