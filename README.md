# Indexus Core

<a href="https://github.com/indexus/go-indexus-core/blob/master/LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue" alt="License"/></a> <a href="https://discord.gg/EXuGtcDgaS"><img src="https://img.shields.io/discord/1235219716738519080?logo=discord&logoColor=white" alt="Discord"/></a>

### Peer-to-Peer Information Access

**go-indexus-core** is the core application of the Indexus protocol, enabling decentralized data indexing and retrieval through a peer-to-peer network. It allows communities to index data and make it available in a decentralized way, focusing on efficient data availability and search capabilities.

---

## Table of Contents

- [Key Features](#key-features)
- [Getting Started](#getting-started)
- [Configuration](#configuration)
- [Architecture](#architecture)
- [API Reference](#api-reference)
- [Contributing](#contributing)
- [License](#license)

## Key Features

- **Decentralized Data Storage**: Distributed storage of collections and items across the network
- **Write-Ahead Logging**: Ensures data durability through operation logging
- **Automatic Data Replication**: Delegates data to other nodes when collections grow too large
- **Binary-Space Partitioning**: Efficient data organization using BST for routing and data management
- **Persistent Storage**: Automatic saving of collections to disk with backup functionality
- **Monitoring Interface**: HTTP endpoints for monitoring node status and network health

## Getting Started

### Prerequisites

- Go 1.22.5 or later
- Git

### Installation

1. Clone the repository:
```bash
git clone https://github.com/indexus/go-indexus-core.git
cd go-indexus-core
```

2. Install dependencies:
```bash
go mod tidy
```

### Running a Node

The project includes several launch configurations:

1. **Run Simulation**:
```bash
go run app/simulation/main.go -port 2100
```

2. **Run Node 1 (Bootstrap)**:
```bash
go run app/node/main.go -p2pPort 21001 -monitoringPort 19001
```

3. **Run Node 2 (Peer)**:
```bash
go run app/node/main.go -bootstrap "127.0.0.1|21001" -p2pPort 21002 -monitoringPort 19002
```

## Architecture

### Core Components

1. **Collections**
   - Manages sets of items with their locations
   - Implements Write-Ahead Logging for durability
   - Automatic data delegation when size thresholds are reached

2. **Node**
   - Handles peer-to-peer communication
   - Manages routing table and network topology
   - Implements Kademlia-like routing algorithm

3. **Worker**
   - Performs periodic tasks:
     - Collection saving (every 5 minutes)
     - Network observation
     - Data refresh and updates
     - Operation replay on startup

4. **Storage**
   - Binary file format for collections
   - Write-Ahead Log for operation durability
   - Automatic backup system

### Data Structures

1. **Binary Search Tree (BST)**
   - Used for routing table management
   - Efficient peer lookup and management
   - Thread-safe implementation

2. **Collection**
   - Manages sets of items
   - Handles data delegation
   - Implements efficient binary encoding

3. **Queue**
   - Thread-safe implementation
   - Used for asynchronous operation processing

## API Reference

### P2P Endpoints

- `POST /ping` - Node discovery and health check
- `GET /neighbors` - Retrieve neighboring nodes
- `GET /random` - Get a random peer
- `POST /transfer` - Transfer items between nodes
- `GET /set` - Retrieve items from a collection
- `POST /item` - Add new items to a collection

### Monitoring Endpoints

- `GET /acknowledged` - List acknowledged nodes
- `GET /registered` - List registered nodes
- `GET /routing` - View routing table
- `GET /ownership` - View owned collections
- `GET /queue` - Check operation queue status

## Data Persistence

The system implements several persistence mechanisms:

1. **Collection Storage**
   - Binary format for efficient storage
   - Automatic periodic saving
   - Backup creation on successful saves

2. **Write-Ahead Log**
   - Records all operations before execution
   - Enables recovery after crashes
   - Cleared after successful collection saves

3. **Backup System**
   - Creates timestamped backups
   - Handles corrupted file recovery
   - Maintains data integrity

## Contributing

1. Fork the repository
2. Create your feature branch (`git checkout -b feature/AmazingFeature`)
3. Commit your changes (`git commit -m 'Add some AmazingFeature'`)
4. Push to the branch (`git push origin feature/AmazingFeature`)
5. Open a Pull Request

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.