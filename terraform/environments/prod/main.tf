module "integrations" {
  source = "../../"

  environment          = var.environment
  account_id           = var.account_id
  github_repo          = var.github_repo
  allowed_environments = var.allowed_environments
  allow_pull_requests  = var.allow_pull_requests
}
