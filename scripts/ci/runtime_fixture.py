"""No-egress HTTPS Kubernetes/Prometheus/dumb-Git fixture, never a real API."""

from functools import partial
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
import json
import math
import ssl
import time
from urllib.parse import parse_qs, urlsplit

COUNTS = {"kubernetes": 0, "prometheus": 0, "git": 0, "writes": 0}


class Handler(SimpleHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def respond(self, value, status=200):
        body = json.dumps(value).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        url = urlsplit(self.path)
        path = url.path
        if path == "/ci-stats":
            return self.respond(COUNTS)
        if path.startswith("/repo.git/"):
            COUNTS["git"] += 1
            return super().do_GET()
        if path.startswith("/api/v1/query"):
            COUNTS["prometheus"] += 1
            params = parse_qs(url.query)
            if path.endswith("query_range"):
                start = float(params["start"][0])
                end = float(params["end"][0])
                step = float(params["step"][0])
                points = min(2048, math.floor((end - start) / step) + 1)
                data = {"resultType": "matrix", "result": [{"metric": {}, "values": [[start + n * step, "0.1"] for n in range(points)]}]}
            else:
                data = {"resultType": "vector", "result": [{"metric": {}, "value": [time.time(), "0.1"]}]}
            return self.respond({"status": "success", "data": data})
        COUNTS["kubernetes"] += 1
        if path == "/api/v1":
            return self.respond({"kind": "APIResourceList", "groupVersion": "v1", "resources": [{"name": "pods/resize", "kind": "Pod", "namespaced": True, "verbs": ["get", "patch", "update"]}]})
        if path == "/apis/apps/v1/namespaces/fixture/deployments/example":
            return self.respond({
                "apiVersion": "apps/v1", "kind": "Deployment",
                "metadata": {"name": "example", "namespace": "fixture", "uid": "fixture-deployment", "generation": 1, "annotations": {"objectset.rio.cattle.io/id": "synthetic-ci"}},
                "spec": {"replicas": 2, "selector": {"matchLabels": {"app": "example"}}, "strategy": {"type": "RollingUpdate", "rollingUpdate": {"maxUnavailable": 0}}, "template": {"metadata": {"labels": {"app": "example"}}, "spec": {"containers": [{"name": "web", "image": "example.invalid/fixture:synthetic", "resources": {"requests": {"cpu": "100m", "memory": "128Mi"}, "limits": {"memory": "256Mi"}}}]}}},
                "status": {"observedGeneration": 1, "replicas": 2, "readyReplicas": 2, "updatedReplicas": 2, "availableReplicas": 2, "unavailableReplicas": 0},
            })
        lists = {
            "/apis/autoscaling/v2/namespaces/fixture/horizontalpodautoscalers": ("autoscaling/v2", "HorizontalPodAutoscalerList"),
            "/apis/policy/v1/namespaces/fixture/poddisruptionbudgets": ("policy/v1", "PodDisruptionBudgetList"),
            "/api/v1/namespaces/fixture/services": ("v1", "ServiceList"),
        }
        if path in lists:
            version, kind = lists[path]
            return self.respond({"apiVersion": version, "kind": kind, "items": []})
        if path == "/api/v1/namespaces/fixture/pods":
            return self.respond({"apiVersion": "v1", "kind": "PodList", "items": [
                {"metadata": {"name": f"example-{n}", "namespace": "fixture", "labels": {"app": "example"}}, "spec": {"nodeName": f"fixture-node-{n}", "containers": [{"name": "web"}]}, "status": {"phase": "Running", "conditions": [{"type": "Ready", "status": "True"}], "containerStatuses": [{"name": "web", "ready": True, "restartCount": 0, "state": {"running": {}}}]}}
                for n in range(2)
            ]})
        return self.respond({"kind": "Status", "status": "Failure", "reason": "NotFound", "code": 404}, 404)

    def reject_write(self):
        COUNTS["writes"] += 1
        self.respond({"error": "fixture forbids Kubernetes and Git HTTP writes"}, 405)

    do_POST = do_PUT = do_PATCH = do_DELETE = reject_write


server = ThreadingHTTPServer(("0.0.0.0", 8443), partial(Handler, directory="/fixture"))
context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
context.load_cert_chain("/fixture/cert.pem", "/fixture/key.pem")
server.socket = context.wrap_socket(server.socket, server_side=True)
server.serve_forever()
