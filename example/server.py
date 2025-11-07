#!/usr/bin/env python3
"""
Mock HTTPS Server for TREM testing
Runs on port 443 with self-signed certificate
"""

import json
import random
import time
import uuid
from http.server import HTTPServer, BaseHTTPRequestHandler
import ssl
import os
import subprocess

class MockAPIHandler(BaseHTTPRequestHandler):
    
    def _send_json(self, status, data):
        self.send_response(status)
        self.send_header('Content-Type', 'application/json')
        self.end_headers()
        self.wfile.write(json.dumps(data).encode())
    
    def do_POST(self):
        content_length = int(self.headers.get('Content-Length', 0))
        body = self.rfile.read(content_length).decode('utf-8')
        
        try:
            data = json.loads(body) if body else {}
        except:
            data = {}
        
        # API 1: Login
        if self.path == '/api/login':
            user_account = random.randint(10000000, 99999999)
            transaction_type = random.choice(['PIX', 'TED'])
            
            response = {
                'userAccount': user_account,
                'transactionType': transaction_type,
                'status': 'authenticated',
                'timestamp': int(time.time())
            }
            
            print(f"[API1] Login -> userAccount={user_account}, type={transaction_type}")
            self._send_json(200, response)
        
        # API 2: Create Transaction
        elif self.path == '/api/transaction':
            report_type = str(uuid.uuid4())
            
            response = {
                'reportType': report_type,
                'status': 'pending',
                'requestedAccount': data.get('userAccount'),
                'requestedType': data.get('transactionType')
            }
            
            print(f"[API2] Transaction -> reportType={report_type}")
            self._send_json(200, response)
        
        # API 3: Get Report
        elif self.path == '/api/report':
            transaction_id = f"{uuid.uuid4()}:{int(time.time())}"
            
            response = {
                'transactionID': transaction_id,
                'reportType': data.get('reportType'),
                'data': 'Transaction completed successfully',
                'amount': random.randint(100, 10000)
            }
            
            print(f"[API3] Report -> transactionID={transaction_id}")
            self._send_json(200, response)
        
        else:
            self._send_json(404, {'error': 'Not found'})
    
    def log_message(self, format, *args):
        # Suppress default logging
        pass


def generate_certificate():
    """Generate self-signed certificate if not exists"""
    if os.path.exists('server.crt') and os.path.exists('server.key'):
        print("[SSL] Using existing certificate")
        return
    
    print("[SSL] Generating self-signed certificate...")
    subprocess.run([
        'openssl', 'req', '-x509', '-newkey', 'rsa:2048',
        '-keyout', 'server.key', '-out', 'server.crt',
        '-days', '365', '-nodes',
        '-subj', '/CN=localhost'
    ], check=True, capture_output=True)
    print("[SSL] Certificate generated: server.crt, server.key")


def main():
    generate_certificate()
    
    server_address = ('0.0.0.0', 443)
    httpd = HTTPServer(server_address, MockAPIHandler)
    
    # Wrap with SSL
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.load_cert_chain('server.crt', 'server.key')
    httpd.socket = context.wrap_socket(httpd.socket, server_side=True)
    
    print("=" * 60)
    print("Mock HTTPS Server Running")
    print("=" * 60)
    print(f"Address: https://localhost:443")
    print("\nEndpoints:")
    print("  POST /api/login       - Returns userAccount + transactionType")
    print("  POST /api/transaction - Returns reportType (UUID)")
    print("  POST /api/report      - Returns transactionID (UUID:epoch)")
    print("\nPress Ctrl+C to stop")
    print("=" * 60)
    
    try:
        httpd.serve_forever()
    except KeyboardInterrupt:
        print("\n[Server] Shutting down...")
        httpd.shutdown()


if __name__ == '__main__':
    main()
