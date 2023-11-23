import subprocess, json
from subprocess import PIPE

def get_list():
    out = subprocess.run(["gsutil", "ls", "gs://prod-eu-west-0-cortex-prod-01-blocks/378660"], stdout=PIPE)
    return out.stdout.decode("utf-8").split("\n")[:-2]

def lookup(link):
    out = subprocess.run(["gsutil", "cat", link+"meta.json"], stdout=PIPE)
    res = json.loads(out.stdout.decode("utf-8"))
    return res["minTime"], res["maxTime"]

def lookup_target(link, target):
    minTime, maxTime = lookup(link)
    if minTime <= target <= maxTime:
        print(link)

def search(target):
    arr = get_list()
    left = 0
    right = len(arr)
    while left < right:
        mid = (left + right) // 2
        minTime, maxTime = lookup(arr[mid])
        print(mid, minTime, maxTime)
        if minTime <= target <= maxTime:
            print(arr[mid])
            break
        elif target < minTime:
            right = mid
        else:
            left = mid + 1
    lookup_target(arr[mid-1], target)
    lookup_target(arr[mid+1], target)

goal = 1688967495000
search(goal)
