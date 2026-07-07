/**
 * CheckoutPage — represents the yunhou consumer-app checkout screen.
 * This is the page that hosts the PayPal button.
 *
 * For tests we don't actually need a real consumer-app install — we mount
 * a minimal checkout HTML page that loads the PayPal SDK directly and
 * hits the yunhou backend's /auth/login + /payments/orders endpoints
 * inline. Each test starts its own server. This is the lightest-weight
 * way to exercise the real PayPal SDK + buyer popup + webhook chain.
 */

import { Page, expect } from '@playwright/test';

export class CheckoutPage {
  constructor(private readonly page: Page) {}

  async goto() {
    await this.page.goto('/checkout.html');
  }

  async expectLoaded() {
    await expect(this.page.locator('#paypal-button-container')).toBeVisible();
  }

  async fillEmail(email: string) {
    await this.page.fill('#user-email', email);
  }

  async clickPaypal() {
    await this.page.click('#paypal-button-container iframe, #paypal-button-container button', {
      timeout: 5000,
    });
  }

  async expectOrderPaid(timeoutMs = 30_000) {
    await expect(this.page.locator('#order-status')).toHaveText('paid', { timeout: timeoutMs });
  }

  async expectSubscriptionActive(timeoutMs = 5_000) {
    await expect(this.page.locator('#sub-status')).toHaveText('active', { timeout: timeoutMs });
  }
}
