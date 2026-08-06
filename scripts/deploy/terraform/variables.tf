variable "aws_region" {
  type    = string
  default = "eu-west-3"
}

variable "project" {
  type    = string
  default = "indexus-aws"
}

variable "network_id" {
  type    = string
  default = "indexus-aws"
}

variable "vpc_id" {
  type = string
}

variable "public_subnet_id" {
  type = string
}

variable "instance_type" {
  type    = string
  default = "t3.micro"
}

variable "ami_id" {
  type        = string
  default     = ""
  description = "Pre-baked Indexus node AMI for spawned instances (see scripts/deploy/scripts/bake-ami.sh)."
}

variable "bootstrap_ami_id" {
  type        = string
  default     = ""
  description = "Optional AMI for the bootstrap instance. Empty = stock AL2023 (userdata installs issuer+node)."
}

variable "worker_count" {
  type        = number
  default     = 0
  description = "Static TF workers. Prefer 0 and let bootstrap autoscale spawn via launch template."
}

variable "spawn_max" {
  type        = number
  default     = 9
  description = "Cost cap on concurrent spawned instances, not a target mesh size."
}

variable "queue_pressure" {
  type        = number
  default     = 200
  description = "Pending work that blocks scale-down. Not a scale-up trigger — scale-up is resource-based (mem/cpu/disk)."
}

variable "pressure_hold" {
  type        = string
  default     = "15s"
  description = "How long a resource pressure signal must last before the node asks for a peer."
}

variable "scale_window" {
  type        = string
  default     = "2m"
  description = "Insert sliding window duration (Go duration). Idleness only."
}

variable "scale_down_threshold" {
  type        = number
  default     = 200
  description = "Downscale when inserts in window fall below this (spawned only)."
}

variable "scale_down_hold" {
  type        = string
  default     = "8m"
  description = "How long quiet condition must hold before downscale (HPA-like stabilization)."
}

variable "scale_cooldown" {
  type        = string
  default     = "3m"
  description = "Quiet period after a scale-up. Must outlast an instance boot and mesh join, or the node asks again before the peer it asked for can take any load."
}

variable "down_cooldown" {
  type        = string
  default     = "2m"
  description = "Quiet period after a terminate before the next SoftLeave may take the drain lock."
}

variable "admin_cidrs" {
  type    = list(string)
  default = ["0.0.0.0/0"]
}

variable "ssh_public_key" {
  type    = string
  default = ""
}

variable "existing_key_name" {
  type    = string
  default = ""
}

variable "tags" {
  type = map(string)
  default = {
    Project = "indexus-aws"
  }
}

variable "delegation" {
  type        = number
  default     = 5000
  description = "Items before Own split (-delegation). Owned zones target ~5k items."
}

variable "transfer_threshold" {
  type        = number
  default     = 200
  description = "Item count at/above which SoftLeave uses snapshot delegation (INDEXUS_TRANSFER_THRESHOLD)."
}
