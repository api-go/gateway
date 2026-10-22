"""Deterministic illustrative workload, not a Gateway throughput benchmark.
100k live IDs; first 1k IDs generate 1000 requests each, others one request.
Compare instant rendezvous expansion with moving the least-loaded fixed buckets.
"""
import hashlib
import json


def digest(value):
    return hashlib.sha256(value.encode()).digest()


def owner(value, count):
    return max(range(count), key=lambda node: digest(f"{value}\0node-{node}"))


sessions = 100000
buckets = 4096
loads = [0] * buckets
members = [0] * buckets
keys = []
for session in range(sessions):
    bucket = int.from_bytes(digest(str(session))[:8], "big") % buckets
    weight = 1000 if session < 1000 else 1
    loads[bucket] += weight
    members[bucket] += 1
    keys.append((bucket, weight, owner(str(session), 3), owner(str(session), 4)))

report = {
    "model": "100000 live IDs, 1000 hot IDs at weight 1000, others weight 1",
    "buckets": buckets,
    "occupiedBuckets": sum(count > 0 for count in members),
    "totalRequestWeight": sum(loads),
    "scenarios": [],
}


def result(name, predicate, state):
    selected = [(bucket, weight) for bucket, weight, old, new in keys if predicate(bucket, old, new)]
    report["scenarios"].append({
        "name": name,
        "movedLiveIDs": len(selected),
        "movedRequestWeight": sum(weight for _, weight in selected),
        "requestWeightPercent": round(100 * sum(weight for _, weight in selected) / sum(loads), 3),
        "requiresSharedOwnershipAndLoadState": state,
    })


result("rendezvous-3-to-4", lambda bucket, old, new: old != new, False)
for count in (41, 1024):
    # Same source-owner fairness: prioritize low load but cap each old owner's
    # bucket donation so one old node cannot donate everything.
    selected = set()
    quota = [count // 3 + (node < count % 3) for node in range(3)]
    for bucket in sorted(range(buckets), key=lambda bucket: (loads[bucket], bucket)):
        source = owner(f"bucket-{bucket}", 3)
        if quota[source] > 0:
            selected.add(bucket)
            quota[source] -= 1
        if len(selected) == count:
            break
    result(f"lowest-load-{count}-buckets", lambda bucket, old, new: bucket in selected, True)

assert report["occupiedBuckets"] == buckets
assert all(item["movedLiveIDs"] > 0 for item in report["scenarios"])
print(json.dumps(report, indent=2))
