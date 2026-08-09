terraform {
  required_version = ">= 1.5.0"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

provider "aws" {
  region = var.aws_region
}

data "aws_ami" "al2023" {
  most_recent = true
  owners      = ["amazon"]
  filter {
    name   = "name"
    values = ["al2023-ami-2023*-x86_64"]
  }
  filter {
    name   = "state"
    values = ["available"]
  }
}

resource "aws_s3_bucket" "artifacts" {
  bucket_prefix = "indexus-aws-artifacts-"
  force_destroy = true
  tags          = var.tags
}

resource "aws_s3_bucket_public_access_block" "artifacts" {
  bucket                  = aws_s3_bucket.artifacts.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# Cost control: expire cold leave dumps / snapshots quickly.
resource "aws_s3_bucket_lifecycle_configuration" "artifacts" {
  bucket = aws_s3_bucket.artifacts.id

  rule {
    id     = "expire-snapshots"
    status = "Enabled"

    filter {
      prefix = "snapshots/"
    }

    expiration {
      days = 3
    }
  }

  rule {
    id     = "abort-multipart"
    status = "Enabled"

    filter {}

    abort_incomplete_multipart_upload {
      days_after_initiation = 1
    }
  }
}

resource "aws_security_group" "node" {
  name_prefix = "indexus-aws-"
  description = "Indexus P2P mesh + issuer + monitoring"
  vpc_id      = var.vpc_id

  ingress {
    description = "P2P"
    from_port   = 21000
    to_port     = 21000
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  ingress {
    description = "Issuer"
    from_port   = 22000
    to_port     = 22000
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  ingress {
    description = "Monitoring"
    from_port   = 19000
    to_port     = 19000
    protocol    = "tcp"
    cidr_blocks = var.admin_cidrs
  }

  ingress {
    description = "SSH"
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = var.admin_cidrs
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = var.tags
}

resource "aws_iam_role" "node" {
  name_prefix = "indexus-aws-node-"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "ec2.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
  tags = var.tags
}

resource "aws_iam_role_policy" "node_s3" {
  name = "artifacts-read"
  role = aws_iam_role.node.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect = "Allow"
        Action = [
          "s3:GetObject",
          "s3:PutObject",
          "s3:ListBucket",
        ]
        Resource = [aws_s3_bucket.artifacts.arn, "${aws_s3_bucket.artifacts.arn}/*"]
      },
      {
        Effect = "Allow"
        Action = [
          "ec2:DescribeInstances",
          "ec2:DescribeTags",
          "ec2:DescribeAddresses",
          "ec2:RunInstances",
          "ec2:CreateTags",
          "ec2:TerminateInstances",
        ]
        Resource = "*"
      },
      {
        Effect   = "Allow"
        Action   = ["iam:PassRole"]
        Resource = aws_iam_role.node.arn
      }
    ]
  })
}

resource "aws_iam_instance_profile" "node" {
  name_prefix = "indexus-aws-node-"
  role        = aws_iam_role.node.name
}

resource "aws_key_pair" "deploy" {
  count      = var.ssh_public_key != "" ? 1 : 0
  key_name   = "${var.project}-deploy"
  public_key = var.ssh_public_key
}

locals {
  key_name = var.ssh_public_key != "" ? aws_key_pair.deploy[0].key_name : var.existing_key_name
  # Baked AMI (ami.auto.tfvars) applies to spawned only — rebaking must not
  # replace the live bootstrap. Bootstrap stays on stock AL2023 unless
  # bootstrap_ami_id is set explicitly.
  spawned_ami_id   = coalesce(var.ami_id != "" ? var.ami_id : null, data.aws_ami.al2023.id)
  bootstrap_ami_id = coalesce(var.bootstrap_ami_id != "" ? var.bootstrap_ami_id : null, data.aws_ami.al2023.id)
}

resource "aws_launch_template" "spawned" {
  name_prefix            = "${var.project}-spawned-"
  image_id               = local.spawned_ami_id
  instance_type          = var.instance_type
  key_name               = local.key_name
  update_default_version = true

  iam_instance_profile {
    name = aws_iam_instance_profile.node.name
  }

  network_interfaces {
    associate_public_ip_address = true
    security_groups             = [aws_security_group.node.id]
    subnet_id                   = var.public_subnet_id
  }

  # gp3 boots faster than default gp2 on many accounts; small root is enough
  # for the prebaked binary + WAL.
  block_device_mappings {
    device_name = "/dev/xvda"
    ebs {
      volume_size           = 8
      volume_type           = "gp3"
      iops                  = 3000
      throughput            = 125
      delete_on_termination = true
    }
  }

  # PreferNear is read from instance tags via IMDS in userdata.
  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 2
    instance_metadata_tags      = "enabled"
  }

  # Bootstrap host is discovered at runtime by tag Role=bootstrap-issuer
  # (avoids circular dependency with bootstrap needing this template id).
  user_data = base64encode(templatefile("${path.module}/userdata_spawned.sh.tftpl", {
    bucket               = aws_s3_bucket.artifacts.bucket
    network_id           = var.network_id
    region               = var.aws_region
    project              = var.project
    delegation           = var.delegation
    queue_pressure       = var.queue_pressure
    pressure_hold        = var.pressure_hold
    scale_window         = var.scale_window
    scale_down_threshold = var.scale_down_threshold
    scale_down_hold      = var.scale_down_hold
    scale_cooldown       = var.scale_cooldown
  }))

  tag_specifications {
    resource_type = "instance"
    tags = merge(var.tags, {
      Name = "${var.project}-spawned"
      Role = "spawned"
    })
  }

  tags = var.tags
}

resource "aws_instance" "bootstrap" {
  ami                         = local.bootstrap_ami_id
  instance_type               = var.instance_type
  subnet_id                   = var.public_subnet_id
  vpc_security_group_ids      = [aws_security_group.node.id]
  iam_instance_profile        = aws_iam_instance_profile.node.name
  key_name                    = local.key_name
  associate_public_ip_address = true
  user_data_replace_on_change = true

  user_data = templatefile("${path.module}/userdata_bootstrap.sh.tftpl", {
    bucket               = aws_s3_bucket.artifacts.bucket
    network_id           = var.network_id
    region               = var.aws_region
    project              = var.project
    delegation           = var.delegation
    launch_template_id   = aws_launch_template.spawned.id
    spawn_max            = var.spawn_max
    queue_pressure       = var.queue_pressure
    pressure_hold        = var.pressure_hold
    scale_window         = var.scale_window
    scale_down_threshold = var.scale_down_threshold
    scale_down_hold      = var.scale_down_hold
    scale_cooldown       = var.scale_cooldown
    down_cooldown        = var.down_cooldown
  })

  tags = merge(var.tags, {
    Name = "${var.project}-bootstrap"
    Role = "bootstrap-issuer"
  })

  # Rebaking spawned AMIs must not replace the live bootstrap.
  lifecycle {
    ignore_changes = [ami]
  }

  depends_on = [aws_launch_template.spawned]
}

resource "aws_instance" "worker" {
  count                       = var.worker_count
  ami                         = local.spawned_ami_id
  instance_type               = var.instance_type
  subnet_id                   = var.public_subnet_id
  vpc_security_group_ids      = [aws_security_group.node.id]
  iam_instance_profile        = aws_iam_instance_profile.node.name
  key_name                    = local.key_name
  associate_public_ip_address = true

  user_data = templatefile("${path.module}/userdata_worker.sh.tftpl", {
    bucket         = aws_s3_bucket.artifacts.bucket
    network_id     = var.network_id
    region         = var.aws_region
    bootstrap_host     = aws_instance.bootstrap.public_ip
    issuer_url         = "http://${aws_instance.bootstrap.public_ip}:22000"
    delegation         = var.delegation
  })

  depends_on = [aws_instance.bootstrap]

  tags = merge(var.tags, {
    Name = "${var.project}-worker-${count.index}"
    Role = "worker"
  })
}
