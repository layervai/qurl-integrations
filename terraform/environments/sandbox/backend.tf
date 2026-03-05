terraform {
  backend "s3" {
    bucket         = "layerv-terraform-state-730883236711"
    key            = "qurl-integrations/terraform.tfstate"
    region         = "us-east-2"
    dynamodb_table = "terraform-locks"
    encrypt        = true
  }
}
