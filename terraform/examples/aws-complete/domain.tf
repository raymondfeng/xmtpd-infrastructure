
# This domain must ALREADY BE REGISTERED in the AWS account.
# Terraform will then take control of the registered domain

data "aws_route53_zone" "public" {
  name         = var.domain_name      # Must end with a dot, e.g. "example.com."
}

data "aws_acm_certificate" "public" {
  domain   = var.domain_name
  statuses = ["ISSUED"]

  most_recent = true
  types       = ["AMAZON_ISSUED"]
}

# Create DNS record for the load balancer
resource "aws_route53_record" "lb" {
  zone_id = data.aws_route53_zone.public.zone_id
  name    = "xmtpd.${var.domain_name}"
  type    = "A"

  alias {
    name                   = module.xmtpd_api.load_balancer_address
    zone_id                = module.xmtpd_api.load_balancer_zone_id
    evaluate_target_health = true
  }
}
