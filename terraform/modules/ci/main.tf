# CI Module — GitHub Actions OIDC Provider + IAM Role
#
# Creates the OIDC identity provider and an IAM role that GitHub Actions
# workflows can assume via OIDC federation. Permissions are scoped to
# Lambda, API Gateway, CloudWatch, SSM, Secrets Manager, and S3 (state).

data "aws_caller_identity" "current" {}
data "aws_region" "current" {}

# -----------------------------------------------------------------------------
# OIDC Provider
# -----------------------------------------------------------------------------

resource "aws_iam_openid_connect_provider" "github" {
  url             = "https://token.actions.githubusercontent.com"
  client_id_list  = ["sts.amazonaws.com"]
  thumbprint_list = ["6938fd4d98bab03faadb97b34396831e3780aea1"]

  tags = {
    Name      = "GitHub Actions OIDC"
    ManagedBy = "Terraform"
  }
}

# -----------------------------------------------------------------------------
# IAM Role — GitHub Actions
# -----------------------------------------------------------------------------

locals {
  role_name = "integrations-${var.environment}-github-actions"

  # Build the list of allowed OIDC sub claims
  environment_conditions = [
    for env in var.allowed_environments :
    "repo:${var.github_repo}:environment:${env}"
  ]

  ref_conditions = ["repo:${var.github_repo}:ref:refs/heads/main"]

  pr_conditions = var.allow_pull_requests ? ["repo:${var.github_repo}:pull_request"] : []

  all_sub_conditions = concat(
    local.environment_conditions,
    local.ref_conditions,
    local.pr_conditions,
  )
}

data "aws_iam_policy_document" "github_actions_trust" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRoleWithWebIdentity"]

    principals {
      type        = "Federated"
      identifiers = [aws_iam_openid_connect_provider.github.arn]
    }

    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:aud"
      values   = ["sts.amazonaws.com"]
    }

    condition {
      test     = "StringLike"
      variable = "token.actions.githubusercontent.com:sub"
      values   = local.all_sub_conditions
    }
  }
}

resource "aws_iam_role" "github_actions" {
  name                 = local.role_name
  assume_role_policy   = data.aws_iam_policy_document.github_actions_trust.json
  max_session_duration = 3600

  tags = {
    Name        = local.role_name
    Environment = var.environment
    ManagedBy   = "Terraform"
  }
}

# -----------------------------------------------------------------------------
# IAM Policies
# -----------------------------------------------------------------------------

# Lambda deployment
data "aws_iam_policy_document" "lambda" {
  statement {
    sid    = "LambdaManagement"
    effect = "Allow"
    actions = [
      "lambda:CreateFunction",
      "lambda:UpdateFunctionCode",
      "lambda:UpdateFunctionConfiguration",
      "lambda:GetFunction",
      "lambda:GetFunctionConfiguration",
      "lambda:ListFunctions",
      "lambda:DeleteFunction",
      "lambda:TagResource",
      "lambda:UntagResource",
      "lambda:ListTags",
      "lambda:AddPermission",
      "lambda:RemovePermission",
      "lambda:GetPolicy",
      "lambda:PublishVersion",
      "lambda:CreateAlias",
      "lambda:UpdateAlias",
      "lambda:DeleteAlias",
      "lambda:GetAlias",
    ]
    resources = ["*"]
  }
}

resource "aws_iam_role_policy" "lambda" {
  name   = "lambda-management"
  role   = aws_iam_role.github_actions.id
  policy = data.aws_iam_policy_document.lambda.json
}

# API Gateway
data "aws_iam_policy_document" "apigateway" {
  statement {
    sid    = "ApiGatewayManagement"
    effect = "Allow"
    actions = [
      "apigateway:GET",
      "apigateway:POST",
      "apigateway:PUT",
      "apigateway:PATCH",
      "apigateway:DELETE",
      "apigateway:TagResource",
      "apigateway:UntagResource",
    ]
    resources = ["arn:aws:apigateway:${data.aws_region.current.name}::*"]
  }
}

resource "aws_iam_role_policy" "apigateway" {
  name   = "apigateway-management"
  role   = aws_iam_role.github_actions.id
  policy = data.aws_iam_policy_document.apigateway.json
}

# CloudWatch Logs
data "aws_iam_policy_document" "cloudwatch" {
  statement {
    sid    = "CloudWatchLogs"
    effect = "Allow"
    actions = [
      "logs:CreateLogGroup",
      "logs:CreateLogStream",
      "logs:PutLogEvents",
      "logs:DescribeLogGroups",
      "logs:DescribeLogStreams",
      "logs:DeleteLogGroup",
      "logs:TagResource",
      "logs:UntagResource",
      "logs:PutRetentionPolicy",
    ]
    resources = ["*"]
  }
}

resource "aws_iam_role_policy" "cloudwatch" {
  name   = "cloudwatch-logs"
  role   = aws_iam_role.github_actions.id
  policy = data.aws_iam_policy_document.cloudwatch.json
}

# SSM Parameter Store
data "aws_iam_policy_document" "ssm" {
  statement {
    sid    = "SSMParameters"
    effect = "Allow"
    actions = [
      "ssm:GetParameter",
      "ssm:GetParameters",
      "ssm:PutParameter",
      "ssm:DeleteParameter",
      "ssm:DescribeParameters",
      "ssm:AddTagsToResource",
      "ssm:RemoveTagsFromResource",
      "ssm:ListTagsForResource",
    ]
    resources = [
      "arn:aws:ssm:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:parameter/integrations/*",
    ]
  }
}

resource "aws_iam_role_policy" "ssm" {
  name   = "ssm-parameters"
  role   = aws_iam_role.github_actions.id
  policy = data.aws_iam_policy_document.ssm.json
}

# Secrets Manager
data "aws_iam_policy_document" "secrets" {
  statement {
    sid    = "SecretsManager"
    effect = "Allow"
    actions = [
      "secretsmanager:CreateSecret",
      "secretsmanager:GetSecretValue",
      "secretsmanager:DescribeSecret",
      "secretsmanager:PutSecretValue",
      "secretsmanager:UpdateSecret",
      "secretsmanager:DeleteSecret",
      "secretsmanager:TagResource",
      "secretsmanager:UntagResource",
    ]
    resources = [
      "arn:aws:secretsmanager:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:secret:integrations/*",
    ]
  }
}

resource "aws_iam_role_policy" "secrets" {
  name   = "secrets-manager"
  role   = aws_iam_role.github_actions.id
  policy = data.aws_iam_policy_document.secrets.json
}

# S3 — Terraform state + deploy artifacts
data "aws_iam_policy_document" "s3" {
  statement {
    sid    = "TerraformState"
    effect = "Allow"
    actions = [
      "s3:GetObject",
      "s3:PutObject",
      "s3:DeleteObject",
      "s3:ListBucket",
    ]
    resources = [
      "arn:aws:s3:::${var.state_bucket}",
      "arn:aws:s3:::${var.state_bucket}/*",
    ]
  }
}

resource "aws_iam_role_policy" "s3" {
  name   = "s3-state"
  role   = aws_iam_role.github_actions.id
  policy = data.aws_iam_policy_document.s3.json
}

# DynamoDB — Terraform state locking
data "aws_iam_policy_document" "dynamodb" {
  statement {
    sid    = "TerraformLocks"
    effect = "Allow"
    actions = [
      "dynamodb:GetItem",
      "dynamodb:PutItem",
      "dynamodb:DeleteItem",
    ]
    resources = [
      "arn:aws:dynamodb:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:table/terraform-locks",
    ]
  }
}

resource "aws_iam_role_policy" "dynamodb" {
  name   = "dynamodb-locks"
  role   = aws_iam_role.github_actions.id
  policy = data.aws_iam_policy_document.dynamodb.json
}

# IAM — scoped to own role and Lambda execution role
data "aws_iam_policy_document" "iam" {
  statement {
    sid    = "IAMManagement"
    effect = "Allow"
    actions = [
      "iam:CreateRole",
      "iam:DeleteRole",
      "iam:GetRole",
      "iam:UpdateRole",
      "iam:TagRole",
      "iam:UntagRole",
      "iam:ListRoleTags",
      "iam:AttachRolePolicy",
      "iam:DetachRolePolicy",
      "iam:PutRolePolicy",
      "iam:DeleteRolePolicy",
      "iam:GetRolePolicy",
      "iam:ListRolePolicies",
      "iam:ListAttachedRolePolicies",
      "iam:CreatePolicy",
      "iam:DeletePolicy",
      "iam:GetPolicy",
      "iam:GetPolicyVersion",
      "iam:ListPolicyVersions",
      "iam:CreatePolicyVersion",
      "iam:DeletePolicyVersion",
      "iam:ListEntitiesForPolicy",
      "iam:PassRole",
    ]
    resources = [
      "arn:aws:iam::${data.aws_caller_identity.current.account_id}:role/integrations-*",
      "arn:aws:iam::${data.aws_caller_identity.current.account_id}:policy/integrations-*",
    ]
  }
}

resource "aws_iam_role_policy" "iam" {
  name   = "iam-management"
  role   = aws_iam_role.github_actions.id
  policy = data.aws_iam_policy_document.iam.json
}
