/**
 * PayPalPopupPage — represents the PayPal sandbox login/payment popup.
 *
 * PayPal SDK opens either an iframe (preferred) or a new window/popup
 * depending on the device flow. We handle both via Playwright's
 * `page.on('popup', ...)` event + `frameLocator`.
 */

import { FrameLocator, Locator, Page, expect } from '@playwright/test';
import { SandboxEnv } from '../helpers/env';

export interface PayPalPopupResult {
  /** True if approval button clicked successfully; false if cancelled. */
  approved: boolean;
  /** Path through the popup that was hit. */
  flow: 'login-then-approve' | 'already-logged-in-approve' | 'declined' | 'cancelled';
}

export class PayPalPopupPage {
  // Lazily resolved — the iframe doesn't exist at construction time
  // (PayPal SDK loads it asynchronously after script execution).
  readonly root: () => FrameLocator;
  readonly page: Page;

  constructor(page: Page, _env: SandboxEnv) {
    this.page = page;
    this.root = () => page.frameLocator('iframe[name^="__paypal"], iframe[src*="paypal"], iframe[id^="__paypal"]').first();
  }

  /** Some tests prefer a real popup window over an iframe. */
  async waitForPopup(): Promise<Page> {
    const popup = await this.page.waitForEvent('popup', { timeout: 30_000 });
    return popup;
  }

  async loginIfNeeded(env: SandboxEnv) {
    // PayPal's "returning customer" path may skip the login form. Detect
    // and adapt: if email field is present, fill it; otherwise skip.
    const emailInput = this.root().locator('input[name="email"], input#email');
    const visible = await emailInput.count();
    if (visible === 0) {
      return { loggedIn: true } as const;
    }
    await emailInput.first().fill(env.buyerEmail);
    const passwordInput = this.root().locator('input[name="password"], input#password');
    await passwordInput.first().fill(env.buyerPassword);
    await this.root().locator('button[type="submit"], button#btnLogin').first().click();
    await this.page.waitForTimeout(2_000);
    return { loggedIn: true } as const;
  }

  async approve(): Promise<PayPalPopupResult> {
    const approveBtn = this.root().locator(
      'button[data-testid="confirmButton"], button#confirmButton, button:has-text("Pay"), button:has-text("Subscribe"), button:has-text("Continue"), button:has-text("Agree & Subscribe")',
    );
    const count = await approveBtn.count();
    if (count === 0) {
      await this.root().locator('button[type="submit"]').first().click();
    } else {
      await approveBtn.first().click();
    }
    await this.page.waitForTimeout(2_000);
    return { approved: true, flow: 'login-then-approve' as const };
  }

  async decline(): Promise<PayPalPopupResult> {
    const cancel = this.root().locator(
      'button:has-text("Cancel"), a:has-text("Cancel"), button:has-text("Go back")',
    ).first();
    const count = await cancel.count();
    if (count === 0) {
      await this.page.keyboard.press('Escape');
    } else {
      await cancel.click();
    }
    return { approved: false, flow: 'cancelled' };
  }
}
