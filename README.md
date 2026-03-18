<p align="center">
  <img src="logo.png" alt="Bar Meet Tunnel Logo" width="300px">
</p>

# 🌌 Bar Meet Tunnel

<p align="center">
  <img src="https://img.shields.io/badge/go-%2300ADD8.svg?style=for-the-badge&logo=go&logoColor=white" alt="Go">
  <img src="https://img.shields.io/badge/redis-%23DD0031.svg?style=for-the-badge&logo=redis&logoColor=white" alt="Redis">
  <img src="https://img.shields.io/badge/nginx-%23009639.svg?style=for-the-badge&logo=nginx&logoColor=white" alt="Nginx">
  <img src="https://img.shields.io/badge/docker-%230db7ed.svg?style=for-the-badge&logo=docker&logoColor=white" alt="Docker">
  <br>
  <img src="https://img.shields.io/badge/license-MIT-blue.svg" alt="License MIT">
</p>

**Pro-Level HTTP/2 Tunneling.** Create and access your own secure tunnel like Cloudflare, built from scratch with Go, Redis, and Nginx.

---

## 🏗️ Architecture
- **Gateway Server**: Core logic for routing public traffic to agents.
- **Client Agent**: Connects your local server to the Gateway.
- **Redis Session Mapping**: Fast, scalable mapping of `subdomain -> agent_id`.
- **Nginx & TLS**: Professional-grade security with HTTP/2 termination.

## 🚀 Getting Started

### 1. Prerequisites
- Docker & Docker Compose
- Go 1.21+ (For the Agent)

### 2. Launch Infrastructure
```bash
# In the project root:
docker-compose up -d
```
This starts the **Gateway**, **Redis**, and **Nginx**.

### 3. Run the Agent (Local Machine)
Connect your local service (default: `localhost:8080`) to the tunnel:
```bash
cd agent
go run main.go
```

### 🌍 Access Your Tunnel
Requests to `bar-meet-app.tunnel.com` will now stream directly to your local machine!

---

## 💎 Features
- ✅ **HTTP/2 Multiplexing**: High efficiency, low latency.
- ✅ **Secure TLS**: Nginx-powered SSL termination.
- ✅ **Redis Backed**: Persistent and scalable session management.
- ✅ **Modern Architecture**: Clean-code and Pro-level patterns.

---

---

## 📄 License
This project is licensed under the [MIT License](LICENSE).

"Secure Local Access, Redefined."
