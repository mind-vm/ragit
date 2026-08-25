# Northwind Support Handbook

The reference the support desk answers from. Everything here is fictional and
written for these examples — no customer data, nothing copied.

## Accounts

### Resetting a password

A customer who cannot sign in resets their own password from the sign-in page.
Send them to **Sign in → Forgot password** and have them enter the address on
the account. The reset link is valid for 30 minutes and can only be used once.

If the link has expired, the customer requests a new one; support cannot issue
a reset link on a customer's behalf, and cannot set a password directly. This
is deliberate: a password support can set is a password support can be
socially engineered into setting.

### Locked accounts

Five failed sign-in attempts lock an account for 15 minutes. The lock clears on
its own. Support can clear it early from the admin console, but should confirm
the caller's identity against the last four digits of the payment method first.

### Changing the account email

The customer changes it themselves under **Settings → Account**. A confirmation
goes to both the old and the new address, and the change only takes effect once
the new address is confirmed.

## Billing

### Refunds

Orders can be refunded in full within 30 days of delivery. After 30 days a
refund needs a supervisor's approval, recorded in the order notes.

Partial refunds are for damaged items only. A customer who simply wants fewer
of something returns the extras and is refunded on receipt.

### Failed payments

A failed charge is retried after 24 hours, then after 72 hours. After the third
failure the subscription moves to past_due and the customer is emailed. The
account keeps working through the retry window — access is never cut off before
the third failure.

### Invoices

Invoices are generated on the first of the month for the month just ended. A
customer who needs a VAT number added should have it entered under
**Settings → Billing** before the first, since an issued invoice cannot be
edited, only credited and reissued.

## Shipping

### Delivery windows

Standard delivery is 3-5 working days. Express is next working day for orders
placed before 14:00. Neither figure includes the day the order was placed.

### Damaged deliveries

Photograph the damage before anything is returned. A damaged-on-arrival claim
without a photograph needs supervisor approval, which slows the refund down by
about two days — so it is worth asking for the photograph first, gently.

### Missing parcels

A parcel is not considered missing until 10 working days after dispatch, which
is when the carrier's own trace process opens. Before that, reassure and wait.
