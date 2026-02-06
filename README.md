# Lineblocs Task Scheduler

An enterprise-grade task orchestration system for **Lineblocs**. This service manages critical background operations including billing cycles, telecommunications maintenance, and automated notifications using a **Distributor-Worker** pattern backed by RabbitMQ.

## 🏗 Architecture Overview

This repository has been refactored from a script-based crontab system to a **Distributed Task Queue**.

* **The Distributor (`cmd/distributor`):** A lightweight process triggered by a system cron. It identifies which entities (workspaces/users) require action and publishes a "Task" to RabbitMQ.
* **The Worker (`cmd/worker`):** A long-running consumer service that pulls tasks from the queue and executes the heavy business logic (e.g., payment processing, CDR aggregation).

---

## 📁 Directory Hierarchy

```text
.
├── cmd/
│   ├── distributor/
│   │   └── main.go       <-- Periodic Trigger (scans DB, sends to RMQ)
│   └── worker/
│       └── main.go       <-- Persistent Consumer (processes logic)
├── internal/
│   ├── billing/          <-- Consolidated billing (Monthly & Annual)
│   │   ├── service.go    <-- Primary billing orchestration logic
│   │   ├── handlers/     <-- Payment Gateway integrations (Stripe/Braintree)
│   │   └── retry.go      <-- Logic for failed payment handling
│   ├── email/
│   │   └── sender.go     <-- Background email processing
│   ├── maintenance/
│   │   ├── cleanup.go    <-- App cleanup logic
│   │   └── logs.go       <-- Log rotation/removal logic
│   ├── repository/       <-- DB access layer (Payment, Workspace)
│   ├── models/           <-- Shared Data Structures & Queue Tasks
│   ├── queue/            <-- RabbitMQ Connection & Channel Helpers
│   └── utils/            <-- Shared utilities and DB connectors
├── Makefile              <-- Build and lifecycle management
├── Dockerfile            <-- Containerization for Workers
├── go.mod
└── README.md

```

---

## 🚀 Getting Started

### 1. Dependencies

* **Go 1.21+**
* **RabbitMQ** (Message Broker)
* **MySQL** (Lineblocs Database)

### 2. Configuration

Create/update your `.env` file or export the following variables:

```bash
# Queue Configuration
QUEUE_URL=amqp://guest:guest@localhost:5672/
BILLING_QUEUE_NAME=billing_tasks

# Logging
export LOG_DESTINATIONS=file,cloudwatch

```

### 3. Build & Run

The project uses a **Makefile** to manage the dual-binary build process.

```bash
# Build both the Distributor and Worker
make build

# Run the Worker (should be managed by Supervisor/Systemd)
./bin/worker

# Run the Distributor (should be triggered by Crontab)
./bin/distributor

```

---

## 💡 Engineering Insights

### Idempotency

Because Workers use **Negative Acknowledgments (Nack)** to retry failed tasks, all logic—especially billing—**must be idempotent**.

* **Rule:** Multiple executions of the same task must result in the user being charged exactly once.
* **Implementation:** Use a composite key `lineblocs_{workspace_id}_{period_date}` as the idempotency token for Stripe/Braintree.

### Scaling the Workers

The system is designed for horizontal scale. If the billing queue grows during the first of the month:

1. Check the queue depth in the RabbitMQ Management UI.
2. Spin up additional instances of the `worker` binary.
3. The `Qos(1)` setting ensures tasks are distributed evenly without overloading a single worker.

### Retries & Dead Letters

* **Transient Failures:** Database locks or network hiccups trigger a requeue.
* **Fatal Failures:** Invalid card tokens or logic errors are logged and moved to a **Dead Letter Queue (DLX)** to prevent infinite retry loops.

---

## 🛠 Maintenance

### Generate Mocks

If you modify interfaces in `repository/` or `handlers/`, regenerate mocks for testing:

```bash
make mock

```

### Linting & Formatting

```bash
# Run golangci-lint
make lint

# Format code to standard
make format

```