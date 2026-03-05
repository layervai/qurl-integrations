variable "environment" {
  description = "Environment name (sandbox or prod)"
  type        = string
  validation {
    condition     = contains(["sandbox", "prod"], var.environment)
    error_message = "Environment must be 'sandbox' or 'prod'."
  }
}

variable "github_repo" {
  description = "GitHub repository in org/repo format"
  type        = string
  default     = "layervai/qurl-integrations"
}

variable "account_id" {
  description = "AWS account ID for this environment"
  type        = string
}

variable "state_bucket" {
  description = "S3 bucket name for Terraform state"
  type        = string
}

variable "allowed_environments" {
  description = "GitHub Actions environments allowed to assume this role"
  type        = list(string)
}

variable "allow_pull_requests" {
  description = "Whether to allow PRs to assume this role (sandbox only)"
  type        = bool
  default     = false
}
