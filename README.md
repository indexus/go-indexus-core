# Indexus Core

<a href="https://github.com/indexus/go-indexus-core/blob/master/LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue" alt="License"/></a> <a href="https://discord.gg/EXuGtcDgaS"><img src="https://img.shields.io/discord/1235219716738519080?logo=discord&logoColor=white" alt="Discord"/></a>

### Peer-to-Peer Information Access

**go-indexus-core** is the core application of the Indexus protocol, enabling decentralized data indexing and retrieval through a peer-to-peer network. It allows communities to index data and make it available in a decentralized way, focusing on efficient data availability and search capabilities.

---

## Table of Contents

- [Indexus Core](#indexus-core)
    - [Peer-to-Peer Information Access](#peer-to-peer-information-access)
  - [Table of Contents](#table-of-contents)
  - [About go-indexus-core](#about-go-indexus-core)
  - [Key Features](#key-features)
    - [Granted](#granted)
    - [Upcoming](#upcoming)
  - [Getting Started](#getting-started)
    - [System Requirements](#system-requirements)
    - [Installation](#installation)
    - [Running the Application](#running-the-application)
  - [Configuration](#configuration)
    - [Example:](#example)
  - [Usage](#usage)
    - [Starting a Node](#starting-a-node)
    - [Connecting to the Network](#connecting-to-the-network)
    - [Monitoring the Node](#monitoring-the-node)
    - [Monitoring the Node](#monitoring-the-node-1)
  - [API Endpoints](#api-endpoints)
    - [Client Endpoints](#client-endpoints)
    - [Peer Endpoints](#peer-endpoints)
    - [Monitoring Endpoints](#monitoring-endpoints)
  - [Contributing](#contributing)
  - [License](#license)
  - [Contact](#contact)
- [Appendix](#appendix)
  - [Sample Output](#sample-output)
  - [Understanding `main.go`](#understanding-maingo)
  - [Notes](#notes)

---

## About go-indexus-core

At the core of the Indexus protocol lies the concept of **collections** and **items**. These entities are indexed and made available by a network of peers. Users can explore and retrieve data by running a proximity algorithm that interacts with the network of nodes.

Each collection is constructed upon a space that encompasses one or multiple dimensions. These dimensions can include geospatiality, temporality, words, and more. By defining coordinates within this space, items are assigned specific locations.

To navigate through the Indexus collections effectively, users employ a combination of origin, filters, and algorithm parameters. By specifying an origin point and setting filters such as directions and distances, users can explore the collections and retrieve relevant items. The proximity algorithm ensures that the displayed feed of items prioritizes proximity, showing the nearest items first. Additionally, the feed can be incrementally loaded without sacrificing performance, allowing users to seamlessly explore an ever-expanding collection of information.

---

## Key Features

### Granted

- **Decentralized Data Indexing**: Index and retrieve data in a decentralized manner without relying on central servers.
- **Peer-to-Peer Networking**: Utilizes a peer-to-peer network based on Kademlia for efficient node communication.
- **Support for Multiple Dimensions**: Index data using various dimensions like geospatiality, temporality, and more.
- **Delegation Mechanism**: Automatically delegates sub-parts of collections to different nodes when they reach a certain size, ensuring balanced data distribution.
- **Extensible and Modular**: Designed to be extensible, allowing for the integration of additional features and dimensions.

### Upcoming

- **Redundancy and Caching**: Implements data redundancy and caching mechanisms for high availability and quick data access.

---

## Getting Started

### System Requirements

- **Go** (version 1.15 or newer)
- **Git**

### Installation

1. **Clone the Repository**

   ```bash
   git clone https://github.com/yourusername/go-indexus-core.git
   ```

2. **Navigate to the Project Directory**

   ```bash
   cd go-indexus-core
   ```

3. **Install Dependencies**

   ```bash
   go mod tidy
   ```

### Running the Application

You can run the application directly using Go:

```bash
go run app/node/main.go
```

Alternatively, you can build the application and run the executable:

```bash
go build -o indexus-core app/node/main.go
./indexus-core
```

---

## Configuration

The application accepts several command-line flags for configuration:

- `-bootstrap`: Host of the bootstrap peer in the format `host|port` (e.g., `bootstrap.indexus.io|21000`).
- `-name`: Name of the node (defaults to a random ID).
- `-monitoringPort`: Port number for the monitoring service (default: `19000`).
- `-p2pPort`: Port number for the peer-to-peer network (default: `21000`).
- `-storage`: Path to the storage directory (default: `.data/backup`).

### Example:

```bash
go run app/node/main.go -bootstrap bootstrap.indexus.io|21000 -name dlLUqr7C9118Ja9etrk_RjN9EMU -p2pPort 21000 -monitoringPort 19000 -storage ./data
```

---

## Usage

### Starting a Node

To start a node on the network, you can use the following commands:

1. **Starting a Node Without a Bootstrap Node**

   If you are starting the first node in the network:

   ```bash
   go run app/node/main.go -name MyFirstNode
   ```

2. **Starting a Node and Connecting to a Bootstrap Node**

   To join an existing network, specify the bootstrap node:

   ```bash
   go run app/node/main.go -bootstrap bootstrap.indexus.io|21000 -name MyNode
   ```

### Connecting to the Network

The node will automatically attempt to connect to the specified bootstrap node and integrate into the network.

### Monitoring the Node

The node includes a monitoring service that runs on the specified monitoring port (default `19000`). You can access the monitoring interface by navigating to:

```
http://localhost:19000/
```

### Monitoring the Node

The node includes a monitoring service that runs on the specified monitoring port (default `19000`). You can access the monitoring interface by navigating to:

```
http://localhost:19000/
```

(Note: Monitoring endpoints and features will be expanded in future releases.)

---

## API Endpoints

### Client Endpoints

1. **Item**

   - **Method:** `POST`
   - **URL:** `http://bootstrap.indexus.io:21000/item`
   - **Body:**

     ```json
     {
       "item": {
         "id": "reference",
         "collection": "oVxwqpn90mkO7ZX9xHCaiskLkTo",
         "location": "rAwbDBzPQPR0e5NXGCDCZXg6d4s"
       },
       "root": "@",
       "current": "rAwbDBzPQPR0e5NXGCDCZXg6d4s"
     }
     ```

   - **Description:** Adds an item to the specified collection at the given location.

2. **Set**

   - **Method:** `GET`
   - **URL:** `http://bootstrap.indexus.io:21000/set`
     - **Query Parameters:**
       - `collection=oVxwqpn90mkO7ZX9xHCaiskLkTo`
       - `location=@`

   - **Description:** Retrieves a set of items from the specified collection and location.

---

### Peer Endpoints

1. **Ping**

   - **Method:** `POST`
   - **URL:** `http://bootstrap.indexus.io:21000/ping`

   - **Description:** Checks the availability of a peer node.

2. **Neighbors**

   - **Method:** `GET`
   - **URL:** `http://bootstrap.indexus.io:21000/neighbors`
     - **Query Parameters:**
       - `origin=rAwbDBzPQPR0e5NXGCDCZXg6d4s`

   - **Description:** Retrieves a list of neighboring peers relative to the specified origin.

---

### Monitoring Endpoints

1. **Acknowledged**

   - **Method:** `GET`
   - **URL:** `http://bootstrap.indexus.io:19000/acknowledged`

   - **Description:** Lists acknowledged nodes in the network.

2. **Registered**

   - **Method:** `GET`
   - **URL:** `http://bootstrap.indexus.io:19000/registered`

   - **Description:** Lists registered nodes in the network.

3. **Routing**

   - **Method:** `GET`
   - **URL:** `http://bootstrap.indexus.io:19000/routing`

   - **Description:** Displays the routing table of the node.

4. **Ownership**

   - **Method:** `GET`
   - **URL:** `http://bootstrap.indexus.io:19000/ownership`

   - **Description:** Shows the collections and items owned by the node.

5. **Queue**

   - **Method:** `GET`
   - **URL:** `http://bootstrap.indexus.io:19000/queue`

   - **Description:** Displays the current task queue of the node.

## Contributing

We welcome contributions from the community! Please follow these steps:

1. **Fork the Repository**
2. **Create a Feature Branch**

   ```bash
   git checkout -b feature/YourFeature
   ```

3. **Commit Your Changes**
4. **Push to Your Fork**
5. **Create a Pull Request**

---

## License

This project is licensed under the [MIT License](LICENSE).

---

## Contact

For any inquiries or support, please contact [contact@indexus.io](mailto:contact@indexus.io).

---

# Appendix

## Sample Output

When you start the application, you will see output similar to:

```
██╗███╗   ██╗██████╗ ███████╗██╗  ██╗██╗   ██╗███████╗
██║████╗  ██║██╔══██╗██╔════╝╚██╗██╔╝██║   ██║██╔════╝
██║██╔██╗ ██║██║  ██║█████╗   ╚███╔╝ ██║   ██║███████╗
██║██║╚██╗██║██║  ██║██╔══╝   ██╔██╗ ██║   ██║╚════██║
██║██║ ╚████║██████╔╝███████╗██╔╝ ██╗╚██████╔╝███████║
╚═╝╚═╝  ╚═══╝╚═════╝ ╚══════╝╚═╝  ╚═╝ ╚═════╝ ╚══════╝


Indexus Version 1.0.0 | Build Date: 2023-10-03 | Commit Hash: abcdef1234567890

Start Time: 2023-10-03 15:04:05
Name: dlLUqr7C9118Ja9etrk_RjN9EMU
Monitoring, P2P Ports: 19000 21000
Bootstrap Nodes: bootstrap.indexus.io:21000
Storage Path: ./data

[Additional logs...]
```

---

## Understanding `main.go`

The `main.go` file is the entry point of the application. It performs the following steps:

1. **Flag Parsing**: Parses command-line flags for configuration.
2. **Display Startup Messages**: Outputs the application banner and configuration details.
3. **Initialize Components**:
   - **Settings**: Creates settings for the node.
   - **Storage**: Initializes storage for data persistence.
   - **Node**: Creates a new node instance with the provided settings and storage.
   - **Monitoring Handler**: Sets up the monitoring HTTP handler.
   - **P2P Handler**: Sets up the peer-to-peer HTTP handler.
   - **Worker**: Initializes a worker for background tasks.
4. **Start Services**: Begins listening on the specified ports and starts background processes.
5. **Graceful Shutdown**: Handles interrupt signals to gracefully shut down the node and clean up resources.

---

## Notes

- **Extensibility**: The architecture is designed to be modular, allowing developers to extend functionalities by integrating new dimensions or features.
- **Data Storage**: The current storage implementation is a mockup for simulation purposes. In production, you should implement a robust storage solution.
- **Error Handling**: Proper error handling and logging are crucial for monitoring the health of your node.

---

*This README was last updated on October 2, 2024.*