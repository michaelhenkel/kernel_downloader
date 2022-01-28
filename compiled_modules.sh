#!/bin/sh
cat /results/kernels.csv | grep "true" | cut -d',' -f1
