output "artifacts_bucket" {
  value = aws_s3_bucket.artifacts.bucket
}

output "bootstrap_public_ip" {
  value = aws_instance.bootstrap.public_ip
}

output "bootstrap_instance_id" {
  value = aws_instance.bootstrap.id
}

output "issuer_url" {
  value = "http://${aws_instance.bootstrap.public_ip}:22000"
}

output "worker_public_ips" {
  value = [for i in aws_instance.worker : i.public_ip]
}

output "ssh_bootstrap" {
  value = "ssh ec2-user@${aws_instance.bootstrap.public_ip}"
}

output "ecr_repository_url" {
  value = aws_ecr_repository.node.repository_url
}

output "launch_template_id" {
  value = aws_launch_template.spawned.id
}
