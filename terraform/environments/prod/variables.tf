variable "environment" {
  type = string
}

variable "account_id" {
  type = string
}

variable "github_repo" {
  type    = string
  default = "layervai/qurl-integrations"
}

variable "allowed_environments" {
  type = list(string)
}

variable "allow_pull_requests" {
  type    = bool
  default = false
}
