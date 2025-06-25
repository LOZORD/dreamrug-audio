#!/usr/bin/python3
import argparse
import json
import math
import sys
import time


_MAX_VALUE = 1024 - 1

def main(args: any):
   start_time = current_millis()
   num_inputs = int(args.sensor_count)
   period_multipler = float(args.period_multiplier)

   while True:
    now_ms = current_millis()
    uptime = now_ms - start_time

    sensors = []
    for i in range(num_inputs):
       period = 1_000.0 * (i + 1) * period_multipler
       sensors.append({
          'name': f'input_{1_000 + i}',
          'value': calculate_input(now_ms, period),
       })

    pld = {
       'upt': int(uptime),
       'sensorData': sensors,
    }

    js = json.dumps(pld)
    sys.stdout.write(js + '\n')
    sys.stdout.flush()
    time.sleep(0.1)

def current_millis() -> int:
   return time.time_ns() // 1_000_000

def calculate_input(now: int, period: float) -> int: 
   return int((math.cos(now / period) / 2 + 0.5) * _MAX_VALUE)

if __name__ == "__main__":
   parser = argparse.ArgumentParser(
       prog='Fakeduino',
       description='Fake implementation of the Dream Rug Arduino program. No sensors needed!',
    )
   parser.add_argument('--sensor_count',
                        default=3, 
                        help='The number of sensors to simulate.')
   parser.add_argument('--period_multiplier',
                        default=1.0,
                        help='The period multipler to use for the sensor output waves.',
                       )
   args = parser.parse_args()
   main(args)