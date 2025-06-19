#!/usr/bin/python3
import time
import math
import json
import sys

_PERIOD = 1 / (10.0 * 1_000.0)
_INPUT1_PERIOD = 1 * _PERIOD
_INPUT2_PERIOD = 1/2.0 * _PERIOD
_INPUT3_PERIOD = 1/3.0 * _PERIOD

_MAX_VALUE = 1024 - 1


def main():
   start_time = current_millis()

   while True:
    now_ms = current_millis()
    uptime = now_ms - start_time

    input1 = calculate_input(now_ms, _INPUT1_PERIOD)
    input2 = calculate_input(now_ms, _INPUT2_PERIOD)
    input3 = calculate_input(now_ms, _INPUT3_PERIOD)

    pld = {
       'utp': int(uptime),
       'sensorData': [
          {'name': 'input_1001', 'value': input1},
          {'name': 'input_1002', 'value': input2},
          {'name': 'input_1003', 'value': input3},
       ],
    }

    js = json.dumps(pld)
    sys.stdout.write(js + '\n')
    sys.stdout.flush()
    time.sleep(.1)

def current_millis() -> int:
   return time.time_ns() // 1_000_000

def calculate_input(now: int, period: float) -> int: 
   return int((math.cos(now * period) / 2 + 0.5) * _MAX_VALUE)

if __name__ == "__main__":
    main()