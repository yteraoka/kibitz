# What kibitz costs and whether it is working, in one place.
#
# The numbers come from the worker's own structured logs rather than from its
# Prometheus endpoint, and that is a decision rather than a shortcut. The
# worker is a Cloud Run worker pool that scales to zero, and an instance may
# live two minutes — shorter than any sensible scrape interval — so a
# scraping collector would miss most of what it measured. A log line, once
# written, is collected whether or not the instance that wrote it still
# exists.
#
# The filters match on the message and the fields rather than on the resource
# type. Worker pools, services and jobs report as different resource types,
# and a filter naming the wrong one matches nothing at all while looking
# perfectly correct.

locals {
  # The two log lines that report a finished run. Both carry repository,
  # model, total_tokens and cost.
  run_filter = <<-EOT
    jsonPayload.msg=("review produced findings" OR "answered a question")
  EOT
}

# What each repository is spending. This is the metric the monthly budget is
# measured against, and the one worth looking at before raising a ceiling.
#
# A run on a model nobody priced logs "unpriced" rather than 0, so those
# entries are excluded here instead of being summed as free.
resource "google_logging_metric" "run_cost" {
  project     = var.project_id
  name        = "${var.name_prefix}/run_cost"
  description = "Estimated cost of one finished review or answer, from the configured model prices."
  filter      = "${local.run_filter} AND NOT jsonPayload.cost=\"unpriced\""

  metric_descriptor {
    metric_kind = "DELTA"
    value_type  = "DOUBLE"
    unit        = "1"

    labels {
      key         = "repository"
      value_type  = "STRING"
      description = "owner/name"
    }
    labels {
      key         = "model"
      value_type  = "STRING"
      description = "The model the run was billed at."
    }
  }

  value_extractor = "EXTRACT(jsonPayload.cost)"
  label_extractors = {
    repository = "EXTRACT(jsonPayload.repository)"
    model      = "EXTRACT(jsonPayload.model)"
  }
}

# Runs that could not be priced. A dashboard that only summed run_cost would
# read a month of unpriced work as a month that cost nothing, so the count of
# what is missing sits next to the total.
resource "google_logging_metric" "unpriced_runs" {
  project     = var.project_id
  name        = "${var.name_prefix}/unpriced_runs"
  description = "Finished runs whose model has no configured price, and which therefore count towards no budget."
  filter      = "${local.run_filter} AND jsonPayload.cost=\"unpriced\""

  metric_descriptor {
    metric_kind = "DELTA"
    value_type  = "INT64"
    unit        = "1"

    labels {
      key        = "model"
      value_type = "STRING"
    }
  }

  label_extractors = {
    model = "EXTRACT(jsonPayload.model)"
  }
}

resource "google_logging_metric" "run_tokens" {
  project     = var.project_id
  name        = "${var.name_prefix}/run_tokens"
  description = "Tokens one finished run consumed, triage included."
  filter      = local.run_filter

  metric_descriptor {
    metric_kind = "DELTA"
    value_type  = "INT64"
    unit        = "1"

    labels {
      key        = "repository"
      value_type = "STRING"
    }
  }

  value_extractor = "EXTRACT(jsonPayload.total_tokens)"
  label_extractors = {
    repository = "EXTRACT(jsonPayload.repository)"
  }
}

# How long the agent took. It is the number that decides whether the worker's
# timeout is in the right place, and the first thing to look at when the
# backlog stops draining.
resource "google_logging_metric" "agent_duration" {
  project     = var.project_id
  name        = "${var.name_prefix}/agent_duration"
  description = "How long the agent ran for one review, in nanoseconds."
  filter      = "jsonPayload.msg=\"review produced findings\""

  metric_descriptor {
    metric_kind = "DELTA"
    value_type  = "DISTRIBUTION"
    # Nanoseconds, because that is what slog writes a duration as and
    # EXTRACT does no arithmetic. The buckets below start at one second so
    # the chart still reads in the units a person thinks in.
    unit = "ns"
  }

  value_extractor = "EXTRACT(jsonPayload.agent_duration)"

  bucket_options {
    exponential_buckets {
      num_finite_buckets = 16
      growth_factor      = 2
      scale              = 1000000000 # one second, in the nanoseconds slog wrote
    }
  }
}

resource "google_logging_metric" "findings_posted" {
  project     = var.project_id
  name        = "${var.name_prefix}/findings_posted"
  description = "Findings a review produced after filtering."
  filter      = "jsonPayload.msg=\"review produced findings\""

  metric_descriptor {
    metric_kind = "DELTA"
    value_type  = "INT64"
    unit        = "1"

    labels {
      key        = "repository"
      value_type = "STRING"
    }
  }

  value_extractor = "EXTRACT(jsonPayload.findings)"
  label_extractors = {
    repository = "EXTRACT(jsonPayload.repository)"
  }
}

# Jobs that ran out of retries. The pull request is told, but nobody watching
# the fleet sees that.
resource "google_logging_metric" "jobs_abandoned" {
  project     = var.project_id
  name        = "${var.name_prefix}/jobs_abandoned"
  description = "Jobs that exhausted their retries and reported the failure on the pull request."
  filter      = "jsonPayload.msg=\"giving up on the job\""

  metric_descriptor {
    metric_kind = "DELTA"
    value_type  = "INT64"
    unit        = "1"

    labels {
      key        = "repository"
      value_type = "STRING"
    }
  }

  label_extractors = {
    repository = "EXTRACT(jsonPayload.repository)"
  }
}

# Reviews stopped because the repository spent its month. Not a failure, but
# the thing to look at when somebody asks why a repository went quiet.
resource "google_logging_metric" "budget_exhausted" {
  project     = var.project_id
  name        = "${var.name_prefix}/budget_exhausted"
  description = "Reviews skipped because the repository has spent its monthly budget."
  filter      = "jsonPayload.msg=\"the repository has spent its budget for the month; skipping\""

  metric_descriptor {
    metric_kind = "DELTA"
    value_type  = "INT64"
    unit        = "1"

    labels {
      key        = "repository"
      value_type = "STRING"
    }
  }

  label_extractors = {
    repository = "EXTRACT(jsonPayload.repository)"
  }
}

resource "google_monitoring_dashboard" "kibitz" {
  project = var.project_id

  dashboard_json = jsonencode({
    displayName = "${var.name_prefix}: reviews, cost and backlog"
    mosaicLayout = {
      columns = 12
      tiles = [
        {
          width = 6, height = 4, xPos = 0, yPos = 0
          widget = {
            title = "Cost per repository (daily)"
            xyChart = {
              dataSets = [{
                timeSeriesQuery = {
                  timeSeriesFilter = {
                    filter = "metric.type=\"logging.googleapis.com/user/${google_logging_metric.run_cost.name}\""
                    aggregation = {
                      alignmentPeriod    = "86400s"
                      perSeriesAligner   = "ALIGN_SUM"
                      crossSeriesReducer = "REDUCE_SUM"
                      groupByFields      = ["metric.label.repository"]
                    }
                  }
                }
                plotType = "STACKED_BAR"
              }]
              yAxis = { label = "cost", scale = "LINEAR" }
            }
          }
        },
        {
          width = 6, height = 4, xPos = 6, yPos = 0
          widget = {
            title = "Tokens per repository (daily)"
            xyChart = {
              dataSets = [{
                timeSeriesQuery = {
                  timeSeriesFilter = {
                    filter = "metric.type=\"logging.googleapis.com/user/${google_logging_metric.run_tokens.name}\""
                    aggregation = {
                      alignmentPeriod    = "86400s"
                      perSeriesAligner   = "ALIGN_SUM"
                      crossSeriesReducer = "REDUCE_SUM"
                      groupByFields      = ["metric.label.repository"]
                    }
                  }
                }
                plotType = "STACKED_BAR"
              }]
              yAxis = { label = "tokens", scale = "LINEAR" }
            }
          }
        },
        {
          width = 6, height = 4, xPos = 0, yPos = 4
          widget = {
            title = "Agent duration (50th / 95th percentile)"
            xyChart = {
              dataSets = [
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"logging.googleapis.com/user/${google_logging_metric.agent_duration.name}\""
                      aggregation = {
                        alignmentPeriod  = "300s"
                        perSeriesAligner = "ALIGN_PERCENTILE_50"
                      }
                    }
                  }
                  plotType = "LINE"
                },
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"logging.googleapis.com/user/${google_logging_metric.agent_duration.name}\""
                      aggregation = {
                        alignmentPeriod  = "300s"
                        perSeriesAligner = "ALIGN_PERCENTILE_95"
                      }
                    }
                  }
                  plotType = "LINE"
                },
              ]
              yAxis = { label = "nanoseconds", scale = "LINEAR" }
            }
          }
        },
        {
          width = 6, height = 4, xPos = 6, yPos = 4
          widget = {
            title = "Findings posted, and jobs given up on"
            xyChart = {
              dataSets = [
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"logging.googleapis.com/user/${google_logging_metric.findings_posted.name}\""
                      aggregation = {
                        alignmentPeriod    = "3600s"
                        perSeriesAligner   = "ALIGN_SUM"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  plotType = "LINE"
                },
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"logging.googleapis.com/user/${google_logging_metric.jobs_abandoned.name}\""
                      aggregation = {
                        alignmentPeriod    = "3600s"
                        perSeriesAligner   = "ALIGN_SUM"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  plotType = "LINE"
                },
              ]
              yAxis = { label = "count", scale = "LINEAR" }
            }
          }
        },
        {
          width = 6, height = 4, xPos = 0, yPos = 8
          widget = {
            title = "Backlog: oldest unacknowledged message"
            xyChart = {
              dataSets = [{
                timeSeriesQuery = {
                  timeSeriesFilter = {
                    filter = join(" AND ", [
                      "resource.type=\"pubsub_subscription\"",
                      "resource.labels.subscription_id=\"${google_pubsub_subscription.worker.name}\"",
                      "metric.type=\"pubsub.googleapis.com/subscription/oldest_unacked_message_age\"",
                    ])
                    aggregation = {
                      alignmentPeriod  = "60s"
                      perSeriesAligner = "ALIGN_MAX"
                    }
                  }
                }
                plotType = "LINE"
              }]
              yAxis = { label = "seconds", scale = "LINEAR" }
            }
          }
        },
        {
          width = 6, height = 4, xPos = 6, yPos = 8
          widget = {
            title = "Dead letter topic (anything here is a lost review)"
            xyChart = {
              dataSets = [{
                timeSeriesQuery = {
                  timeSeriesFilter = {
                    filter = join(" AND ", [
                      "resource.type=\"pubsub_topic\"",
                      "resource.labels.topic_id=\"${google_pubsub_topic.dead_letter.name}\"",
                      "metric.type=\"pubsub.googleapis.com/topic/send_message_operation_count\"",
                    ])
                    aggregation = {
                      alignmentPeriod    = "300s"
                      perSeriesAligner   = "ALIGN_SUM"
                      crossSeriesReducer = "REDUCE_SUM"
                    }
                  }
                }
                plotType = "LINE"
              }]
              yAxis = { label = "messages", scale = "LINEAR" }
            }
          }
        },
        {
          width = 6, height = 4, xPos = 0, yPos = 12
          widget = {
            title = "Worker instances"
            xyChart = {
              dataSets = [{
                timeSeriesQuery = {
                  timeSeriesFilter = {
                    filter = join(" AND ", [
                      "resource.type=\"cloud_run_worker_pool\"",
                      "resource.labels.worker_pool_name=\"${google_cloud_run_v2_worker_pool.worker.name}\"",
                      "metric.type=\"run.googleapis.com/container/instance_count\"",
                    ])
                    aggregation = {
                      alignmentPeriod    = "60s"
                      perSeriesAligner   = "ALIGN_MEAN"
                      crossSeriesReducer = "REDUCE_SUM"
                    }
                  }
                }
                plotType = "STACKED_AREA"
              }]
              yAxis = { label = "instances", scale = "LINEAR" }
            }
          }
        },
        {
          width = 6, height = 4, xPos = 6, yPos = 12
          widget = {
            title = "Runs nobody priced, and repositories out of budget"
            xyChart = {
              dataSets = [
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"logging.googleapis.com/user/${google_logging_metric.unpriced_runs.name}\""
                      aggregation = {
                        alignmentPeriod    = "3600s"
                        perSeriesAligner   = "ALIGN_SUM"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  plotType = "LINE"
                },
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"logging.googleapis.com/user/${google_logging_metric.budget_exhausted.name}\""
                      aggregation = {
                        alignmentPeriod    = "3600s"
                        perSeriesAligner   = "ALIGN_SUM"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  plotType = "LINE"
                },
              ]
              yAxis = { label = "count", scale = "LINEAR" }
            }
          }
        },
      ]
    }
  })
}
