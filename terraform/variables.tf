variable "environment" {
  description = "Environment name (sandbox or prod)"
  type        = string
}

variable "account_id" {
  description = "AWS account ID for this environment"
  type        = string
}

variable "github_repo" {
  description = "GitHub repository in org/repo format"
  type        = string
  default     = "layervai/qurl-integrations"
}

variable "allowed_environments" {
  description = "GitHub Actions environments allowed to assume the CI role"
  type        = list(string)
}

variable "allow_pull_requests" {
  description = "Whether to allow PRs to assume the CI role (sandbox only)"
  type        = bool
  default     = false
}
