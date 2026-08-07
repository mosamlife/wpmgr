# GCP Support Ticket: unannounced billing suspension caused a 9h38m multi-project outage

**Category:** Billing
**Sub-category:** Account suspended / unexpected service disruption
**Billing account:** 014DFB-7D1CC1-058026
**Affected projects:** wpmgr-prod (960937736091), mlm-course, wordspree
**Severity:** production outage, now resolved, root cause unexplained

---

## Summary

On 2026-07-31 at approximately 19:22 UTC, all Cloud Run services across three projects on billing account 014DFB-7D1CC1-058026 began returning HTTP 503, with Cloud Run logging `The request failed because billing is disabled for this project.` Service was not restored until 2026-08-01 05:01 UTC, a total outage of 9 hours 38 minutes, and only after a manual payment was made.

A valid payment method was on file. We received no warning that the account was at risk of suspension, and no notification when it was suspended. We would like to understand why this happened and how to prevent a recurrence.

## Timeline (UTC)

| Time | Event |
|---|---|
| Jul 31 19:22:58 | Last successful request, wpmgr-prod (HTTP 200) |
| Jul 31 19:26:27 | First `billing is disabled` error, wpmgr-prod / wpmgr-web |
| Jul 31 19:48:46 | First `billing is disabled` error, mlm-course / mlm-learn-web |
| Jul 31 21:02:08 | First `billing is disabled` error, wordspree |
| Aug 01 04:44:50 | Last `billing is disabled` error, wpmgr-prod |
| Aug 01 04:45:38 | `gcloud run services update` rejected: `PERMISSION_DENIED: This API method requires billing to be enabled` |
| Aug 01 04:54:32 | Last `billing is disabled` error, mlm-course |
| Aug 01 04:54:42 | `gcloud run` still rejected with the same billing error |
| Aug 01 04:56:24 | Last `billing is disabled` error, wordspree |
| Aug 01 ~04:56 to 05:01 | Manual payment propagated |
| Aug 01 05:01:02 | First successful control-plane write (revision created) |
| Aug 01 05:01:23 | First HTTP 200 served |
| Aug 01 05:02 to 05:14 | All requests returned HTTP 429 `The request was aborted because there was no available instance`, despite the revision reporting `ContainerHealthy: True`, `MinInstancesProvisioned: True`, and a request rate of only about 25/min against a capacity of 4 instances x 80 concurrency |
| Aug 01 05:15:41 | Full service restored and verified |

The staggered first-error times reflect only when each project next received a request, not staggered failure. All three lost service simultaneously.

## Questions

**1. Why was the account suspended?**
A payment method was on file. Please provide the specific reason the account stopped being chargeable on 2026-07-31, and the exact suspension timestamp from your side.

**2. Was any notification sent, and to which address?**
We received no warning before suspension and no alert at the time of suspension. Please confirm whether notifications were sent, to what address, and at what times. If they went to an address other than the account admin's working email, we need to know which.

**3. Why did the project report `billingEnabled: true` throughout the outage?**
This is our main concern. During the entire outage:

```
$ gcloud billing projects describe wpmgr-prod
billingAccountName: billingAccounts/014DFB-7D1CC1-058026
billingEnabled: true
```

while every API call was simultaneously rejected with `BILLING_DISABLED`. The console similarly showed billing as enabled. This directly caused us to spend hours investigating our own infrastructure (load balancer, serverless NEGs, SSL certificates, Cloud Run revisions, Cloud SQL, IAM) before finding the real cause in Cloud Run's runtime logs.

Is there any supported API or console field that reports whether a project is actually chargeable, as opposed to merely linked to a billing account? If not, we would like to file this as a product feedback item: `billingEnabled` reporting `true` for a suspended account is actively misleading during an incident.

**4. Why were requests throttled with HTTP 429 for ~13 minutes after reinstatement?**
Between 05:02 and 05:14 UTC every request returned 429 `no available instance`, even though the revision was healthy with min instances provisioned and traffic was roughly 25 requests/min against a capacity of 320 concurrent. This looks like a project-level throttle applied after reinstatement rather than a capacity issue. Please confirm whether reinstated projects are rate limited, and for how long, so we can set expectations during a future recovery.

**5. Is there a supported way to be alerted on this?**
Our own uptime monitoring runs inside one of the affected projects, so it cannot alert us to this class of failure. Please advise the recommended mechanism for alerting on billing account health, specifically account suspension, distinct from a spend budget alert.

## What we verified was NOT the cause

For completeness, we confirmed all of the following were healthy throughout the outage, so this was not a configuration problem on our side:

- Global external load balancer: forwarding rules, target HTTPS proxy, URL map with correct host rules and path matchers, three backend services, three serverless NEGs correctly bound to their Cloud Run services
- All three managed SSL certificates: ACTIVE
- Cloud Run: all revisions `Ready: True`, `ContainerHealthy: True`
- Cloud SQL instance wpmgr-pg: RUNNABLE
- IAM: `allUsers` holds `roles/run.invoker` on the affected services
- No Google Cloud incident was open for asia-south1 or us-central1 during this window
- No deployment or configuration change was made between the last healthy request and the outage

## Impact

- wpmgr-prod: a production SaaS control plane serving managed WordPress sites. 9h38m unavailable. Connected customer sites could not reach it for the duration.
- mlm-course (skilltycoon) and wordspree: 14 Cloud Run services each, unavailable for the same window.

## Requested outcome

1. A root cause for the suspension.
2. Confirmation of what notifications were sent and where.
3. Guidance, or a product fix, for the misleading `billingEnabled: true` signal.
4. A supported way to alert on billing account suspension.
