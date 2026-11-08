import { expect, test, vi } from "vitest";
import { page } from "vitest/browser";
import { databaseRoleCreated } from "../../mocks/fixtures";
import { renderInFrame } from "../testing/render";
import { PersonalServerAction } from "./PersonalServer";

test("personal server: a 201 is declared, not visible yet; the role name is required", async () => {
  const onCreate = vi.fn(async () => databaseRoleCreated);
  const screen = await renderInFrame(<PersonalServerAction cluster="orders" actor="me" onCreate={onCreate} />);
  await screen.getByRole("button", { name: /Create my personal server/ }).click();
  const dialog = page.getByRole("alertdialog");
  await dialog.getByLabelText(/To confirm, type/).fill("orders");
  await expect.element(dialog.getByRole("button", { name: "Request" })).toBeDisabled();
  await dialog.getByLabelText(/^PostgreSQL role/).fill("alice_ro");
  await dialog.getByLabelText(/^pgAdmin user/).fill("alice@example.com");
  await dialog.getByRole("button", { name: "Request" }).click();
  expect(onCreate).toHaveBeenCalledWith({ role: "alice_ro", pgAdminUser: "alice@example.com" });
  await expect.element(page.getByRole("alertdialog")).not.toBeInTheDocument();
  await expect.element(screen.getByText(/ServersSynced/)).toBeVisible();
  expect(screen.container.textContent).not.toMatch(/password:/i);
  expect(screen.container.textContent).toContain("PgToolBox (personal)");
});
