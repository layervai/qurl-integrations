# Root module — qurl-integrations infrastructure
#
# Called from terraform/environments/{sandbox,prod}/main.tf

terraform {
  required_version = "~> 1.14"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.35"
    }
  }
}

provider "aws" {
  region = "us-east-2"

  default_tags {
    tags = {
      Project     = "qurl-integrations"
      Environment = var.environment
      ManagedBy   = "Terraform"
    }
  }
}

module "ci" {
  source = "./modules/ci"

  environment          = var.environment
  account_id           = var.account_id
  github_repo          = var.github_repo
  state_bucket         = "layerv-terraform-state-${var.account_id}"
  allowed_environments = var.allowed_environments
  allow_pull_requests  = var.allow_pull_requests
}
