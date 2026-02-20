#!/usr/bin/env python3
"""
Mock HTTPS Server for TREM Examples
Supports HTTP/2 and HTTP/1.1 via ALPN negotiation
Includes race condition vulnerable endpoints for demonstration

Requirements:
    pip install h2 hpack

Usage:
    sudo python3 server.py
    
Endpoints:
    POST /api/login      - Returns session token + user info
    POST /api/transfer   - Vulnerable to race condition (balance check)
    POST /api/redeem     - Vulnerable to race condition (coupon single-use)
"""

import json
import random
import time
import uuid
import ssl
import socket
import threading
import os
import subprocess
import sys
from collections import defaultdict

# Try to import h2 for HTTP/2 support
try:
    import h2.connection
    import h2.events
    import h2.config
    H2_AVAILABLE = True
except ImportError:
    H2_AVAILABLE = False
    print("[WARN] h2 not installed. HTTP/2 disabled. Install with: pip install h2")


# ============================================================================
# Simulated Database / State
# ============================================================================

class MockDatabase:
    def __init__(self):
        self.lock = threading.Lock()
        self.users = {
            "admin": {"password": "admin123", "balance": 1000, "id": "user_001"},
            "alice": {"password": "alice123", "balance": 500, "id": "user_002"},
            "bob": {"password": "bob123", "balance": 200, "id": "user_003"},
        }
        self.sessions = {}  # token -> user
        self.coupons = {
            "DISCOUNT50": {"value": 50, "used": False, "uses": 0, "max_uses": 1},
            "BONUS100": {"value": 100, "used": False, "uses": 0, "max_uses": 1},
            "FREEBIE": {"value": 25, "used": False, "uses": 0, "max_uses": 1},
        }
        self.transfer_log = []
        self.redeem_log = []
    
    def login(self, username, password):
        user = self.users.get(username)
        if user and user["password"] == password:
            token = f"tok_{uuid.uuid4().hex[:16]}"
            self.sessions[token] = username
            return {"token": token, "userId": user["id"], "username": username}
        return None
    
    def get_user_by_token(self, token):
        username = self.sessions.get(token)
        if username:
            return self.users.get(username)
        return None
    
    def transfer(self, token, amount, to_account):
        """
        VULNERABLE: Race condition in balance check
        No lock between check and update!
        """
        username = self.sessions.get(token)
        if not username:
            return {"error": "Invalid session", "status": "failed"}
        
        user = self.users[username]
        
        # VULNERABLE: Time gap between check and update
        current_balance = user["balance"]
        
        # Simulate processing delay (makes race easier to exploit)
        time.sleep(0.01)
        
        if current_balance >= amount:
            # VULNERABLE: No lock, balance could have changed
            user["balance"] = current_balance - amount
            
            tx_id = f"TX{int(time.time()*1000)}{random.randint(100,999)}"
            self.transfer_log.append({
                "id": tx_id,
                "from": username,
                "to": to_account,
                "amount": amount,
                "balance_after": user["balance"],
                "timestamp": time.time()
            })
            
            return {
                "status": "success",
                "transactionId": tx_id,
                "amount": amount,
                "newBalance": user["balance"],
                "to": to_account
            }
        else:
            return {
                "status": "failed",
                "error": "Insufficient funds",
                "balance": current_balance,
                "requested": amount
            }
    
    def redeem_coupon(self, token, coupon_code):
        """
        VULNERABLE: Race condition in coupon redemption
        Check and update not atomic!
        """
        username = self.sessions.get(token)
        if not username:
            return {"error": "Invalid session", "status": "failed"}
        
        coupon = self.coupons.get(coupon_code.upper())
        if not coupon:
            return {"error": "Invalid coupon", "status": "failed"}
        
        # VULNERABLE: Check without lock
        if coupon["uses"] >= coupon["max_uses"]:
            return {"error": "Coupon already used", "status": "failed"}
        
        # Simulate processing delay
        time.sleep(0.01)
        
        # VULNERABLE: Update without lock - race window!
        coupon["uses"] += 1
        user = self.users[username]
        user["balance"] += coupon["value"]
        
        redeem_id = f"RD{int(time.time()*1000)}{random.randint(100,999)}"
        self.redeem_log.append({
            "id": redeem_id,
            "user": username,
            "coupon": coupon_code,
            "value": coupon["value"],
            "total_uses": coupon["uses"],
            "timestamp": time.time()
        })
        
        return {
            "status": "success",
            "redeemId": redeem_id,
            "couponValue": coupon["value"],
            "newBalance": user["balance"],
            "couponUses": coupon["uses"]
        }
    
    def get_stats(self):
        """Returns server stats for verification"""
        total_transfers = len(self.transfer_log)
        total_redeems = len(self.redeem_log)
        
        return {
            "users": {u: {"balance": d["balance"]} for u, d in self.users.items()},
            "coupons": {c: {"uses": d["uses"], "max": d["max_uses"]} for c, d in self.coupons.items()},
            "total_transfers": total_transfers,
            "total_redeems": total_redeems,
            "transfers": self.transfer_log[-10:],  # last 10
            "redeems": self.redeem_log[-10:]
        }
    
    def reset(self):
        """Reset state for new test"""
        self.users = {
            "admin": {"password": "admin123", "balance": 1000, "id": "user_001"},
            "alice": {"password": "alice123", "balance": 500, "id": "user_002"},
            "bob": {"password": "bob123", "balance": 200, "id": "user_003"},
        }
        self.sessions = {}
        self.coupons = {
            "DISCOUNT50": {"value": 50, "used": False, "uses": 0, "max_uses": 1},
            "BONUS100": {"value": 100, "used": False, "uses": 0, "max_uses": 1},
            "FREEBIE": {"value": 25, "used": False, "uses": 0, "max_uses": 1},
        }
        self.transfer_log = []
        self.redeem_log = []
        return {"status": "reset", "message": "Server state reset"}


DB = MockDatabase()


# ============================================================================
# Request Router
# ============================================================================

def route_request(method, path, headers, body):
    """Route request to appropriate handler"""
    try:
        data = json.loads(body) if body else {}
    except:
        data = {}
    
    # Extract auth token from various header formats
    auth_header = headers.get("authorization", "")
    token = ""
    if auth_header.startswith("Bearer "):
        token = auth_header[7:]  # Remove "Bearer " prefix
    
    # POST /api/login
    if method == "POST" and path == "/api/login":
        username = data.get("username", "")
        password = data.get("password", "")
        result = DB.login(username, password)
        if result:
            print(f"[LOGIN] {username} -> token={result['token'][:20]}...")
            return 200, result
        else:
            return 401, {"error": "Invalid credentials", "status": "failed"}
    
    # POST /api/transfer
    elif method == "POST" and path == "/api/transfer":
        amount = data.get("amount", 0)
        to_account = data.get("to", "unknown")
        result = DB.transfer(token, amount, to_account)
        status_code = 200 if result.get("status") == "success" else 400
        print(f"[TRANSFER] amount={amount} to={to_account} -> {result.get('status')}")
        return status_code, result
    
    # POST /api/redeem
    elif method == "POST" and path == "/api/redeem":
        coupon_code = data.get("coupon", "")
        result = DB.redeem_coupon(token, coupon_code)
        status_code = 200 if result.get("status") == "success" else 400
        print(f"[REDEEM] coupon={coupon_code} -> {result.get('status')} (uses={result.get('couponUses', 'N/A')})")
        return status_code, result
    
    # GET /api/stats
    elif method == "GET" and path == "/api/stats":
        return 200, DB.get_stats()
    
    # POST /api/reset
    elif method == "POST" and path == "/api/reset":
        return 200, DB.reset()
    
    # Not found
    else:
        return 404, {"error": "Not found", "path": path}


# ============================================================================
# HTTP/1.1 Handler
# ============================================================================

def handle_http1(conn, addr):
    """Handle HTTP/1.1 connection"""
    try:
        while True:
            # Read request
            data = b""
            while b"\r\n\r\n" not in data:
                chunk = conn.recv(4096)
                if not chunk:
                    return
                data += chunk
            
            # Parse headers
            header_end = data.index(b"\r\n\r\n")
            header_data = data[:header_end].decode("utf-8")
            body_start = data[header_end + 4:]
            
            lines = header_data.split("\r\n")
            request_line = lines[0]
            parts = request_line.split(" ")
            method = parts[0]
            path = parts[1] if len(parts) > 1 else "/"
            
            headers = {}
            content_length = 0
            for line in lines[1:]:
                if ":" in line:
                    key, value = line.split(":", 1)
                    headers[key.strip().lower()] = value.strip()
                    if key.strip().lower() == "content-length":
                        content_length = int(value.strip())
            
            # Read body if needed
            body = body_start
            while len(body) < content_length:
                chunk = conn.recv(4096)
                if not chunk:
                    break
                body += chunk
            body = body[:content_length].decode("utf-8") if body else ""
            
            # Route and get response
            status_code, response_data = route_request(method, path, headers, body)
            response_body = json.dumps(response_data).encode("utf-8")
            
            # Send response
            status_text = {200: "OK", 400: "Bad Request", 401: "Unauthorized", 404: "Not Found"}.get(status_code, "OK")
            response = f"HTTP/1.1 {status_code} {status_text}\r\n"
            response += f"Content-Type: application/json\r\n"
            response += f"Content-Length: {len(response_body)}\r\n"
            response += f"Connection: keep-alive\r\n"
            response += f"\r\n"
            conn.sendall(response.encode("utf-8") + response_body)
            
            # Check Connection header
            if headers.get("connection", "").lower() == "close":
                break
                
    except Exception as e:
        if "Connection reset" not in str(e) and "Broken pipe" not in str(e):
            print(f"[HTTP/1.1] Error: {e}")
    finally:
        conn.close()


# ============================================================================
# HTTP/2 Handler
# ============================================================================

def handle_http2(conn, addr):
    """Handle HTTP/2 connection"""
    if not H2_AVAILABLE:
        return
    
    config = h2.config.H2Configuration(client_side=False)
    h2_conn = h2.connection.H2Connection(config=config)
    h2_conn.initiate_connection()
    conn.sendall(h2_conn.data_to_send())
    
    # Track stream data
    streams = {}  # stream_id -> {"headers": {}, "body": b""}
    
    try:
        while True:
            data = conn.recv(65535)
            if not data:
                break
            
            events = h2_conn.receive_data(data)
            
            for event in events:
                if isinstance(event, h2.events.RequestReceived):
                    stream_id = event.stream_id
                    # Convert headers from list of tuples (may be bytes) to dict of strings
                    headers = {}
                    for k, v in event.headers:
                        # Handle both bytes and string keys/values
                        if isinstance(k, bytes):
                            k = k.decode('utf-8')
                        if isinstance(v, bytes):
                            v = v.decode('utf-8')
                        headers[k] = v
                    streams[stream_id] = {"headers": headers, "body": b""}
                
                elif isinstance(event, h2.events.DataReceived):
                    stream_id = event.stream_id
                    if stream_id in streams:
                        streams[stream_id]["body"] += event.data
                    h2_conn.acknowledge_received_data(len(event.data), stream_id)
                
                elif isinstance(event, h2.events.StreamEnded):
                    stream_id = event.stream_id
                    if stream_id in streams:
                        stream_data = streams[stream_id]
                        headers = stream_data["headers"]
                        body = stream_data["body"].decode("utf-8") if stream_data["body"] else ""
                        
                        method = headers.get(":method", "GET")
                        path = headers.get(":path", "/")
                        
                        # Convert pseudo-headers to regular headers
                        req_headers = {}
                        for k, v in headers.items():
                            if not k.startswith(":"):
                                req_headers[k] = v
                        
                        status_code, response_data = route_request(method, path, req_headers, body)
                        response_body = json.dumps(response_data).encode("utf-8")
                        
                        response_headers = [
                            (":status", str(status_code)),
                            ("content-type", "application/json"),
                            ("content-length", str(len(response_body))),
                        ]
                        
                        h2_conn.send_headers(stream_id, response_headers)
                        h2_conn.send_data(stream_id, response_body, end_stream=True)
                        
                        del streams[stream_id]
                
                elif isinstance(event, h2.events.ConnectionTerminated):
                    return
            
            # Send any pending data
            data_to_send = h2_conn.data_to_send()
            if data_to_send:
                conn.sendall(data_to_send)
                
    except Exception as e:
        if "Connection reset" not in str(e) and "Broken pipe" not in str(e):
            print(f"[HTTP/2] Error: {e}")
    finally:
        conn.close()


# ============================================================================
# TLS/ALPN Handler
# ============================================================================

def handle_client(conn, addr, ssl_context):
    """Handle incoming connection with ALPN negotiation"""
    try:
        ssl_conn = ssl_context.wrap_socket(conn, server_side=True)
        protocol = ssl_conn.selected_alpn_protocol()
        
        if protocol == "h2" and H2_AVAILABLE:
            handle_http2(ssl_conn, addr)
        else:
            handle_http1(ssl_conn, addr)
            
    except ssl.SSLError as e:
        if "WRONG_VERSION_NUMBER" not in str(e):
            print(f"[SSL] Error: {e}")
    except Exception as e:
        if "Connection reset" not in str(e):
            print(f"[Client] Error: {e}")


# ============================================================================
# Certificate Generation
# ============================================================================

def generate_certificate():
    """Generate self-signed certificate if not exists"""
    cert_file = "server.crt"
    key_file = "server.key"
    
    if os.path.exists(cert_file) and os.path.exists(key_file):
        print("[SSL] Using existing certificate")
        return cert_file, key_file
    
    print("[SSL] Generating self-signed certificate...")
    subprocess.run([
        "openssl", "req", "-x509", "-newkey", "rsa:2048",
        "-keyout", key_file, "-out", cert_file,
        "-days", "365", "-nodes",
        "-subj", "/CN=localhost"
    ], check=True, capture_output=True)
    print(f"[SSL] Certificate generated: {cert_file}, {key_file}")
    return cert_file, key_file


# ============================================================================
# Main Server
# ============================================================================

def main():
    cert_file, key_file = generate_certificate()
    
    # Setup SSL context with ALPN
    ssl_context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ssl_context.load_cert_chain(cert_file, key_file)
    
    # Set ALPN protocols (h2 first for HTTP/2 preference)
    if H2_AVAILABLE:
        ssl_context.set_alpn_protocols(["h2", "http/1.1"])
    else:
        ssl_context.set_alpn_protocols(["http/1.1"])
    
    # Create server socket
    server = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    server.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    if len(sys.argv) > 1:    	
    		port= int(sys.argv[1])
    	
    else:    	
    		port= 443   
    server.bind(("0.0.0.0", port))
    server.listen(100)
    
    print("=" * 70)
    print("TREM Example Server - Race Condition Vulnerable API")
    print("=" * 70)
    print(f"Address: https://localhost:" + str(port))
    print(f"HTTP/2:  {'Enabled' if H2_AVAILABLE else 'Disabled (install h2: pip install h2)'}")
    print()
    print("Endpoints:")
    print("  POST /api/login    - Authenticate (username, password)")
    print("  POST /api/transfer - Transfer funds (amount, to) [RACE VULNERABLE]")
    print("  POST /api/redeem   - Redeem coupon (coupon) [RACE VULNERABLE]")
    print("  GET  /api/stats    - View server state")
    print("  POST /api/reset    - Reset server state")
    print()
    print("Test Users: admin/admin123, alice/alice123, bob/bob123")
    print("Test Coupons: DISCOUNT50, BONUS100, FREEBIE (single-use)")
    print()
    print("Press Ctrl+C to stop")
    print("=" * 70)
    
    try:
        while True:
            conn, addr = server.accept()
            thread = threading.Thread(target=handle_client, args=(conn, addr, ssl_context))
            thread.daemon = True
            thread.start()
    except KeyboardInterrupt:
        print("\n[Server] Shutting down...")
        server.close()


if __name__ == "__main__":
    main()
