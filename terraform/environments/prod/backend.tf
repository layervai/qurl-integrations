terraform {
  backend "s3" {
    bucket         = "layerv-terraform-state-886375649402"
    key            = "qurl-integrations/terraform.tfstate"
    region         = "us-east-2"
    dynamodb_table = "terraform-locks"
    encrypt        = true
  }
}
