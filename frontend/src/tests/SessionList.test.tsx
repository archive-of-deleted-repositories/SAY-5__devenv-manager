import { describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import { SessionList } from "../components/SessionList";

const baseSession = {
  id: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
  image: "alpine:3.20",
  container_id: "cid",
  container_state: "running",
  created_at: new Date().toISOString(),
  expires_at: new Date(Date.now() + 60_000).toISOString(),
};

describe("SessionList", () => {
  it("shows empty state when no sessions", () => {
    render(
      <SessionList
        sessions={[]}
        onSelect={() => {}}
        onRefresh={() => {}}
        onDelete={() => {}}
      />,
    );
    expect(screen.getByText(/no sessions yet/i)).toBeInTheDocument();
  });

  it("renders session id, image and TTL", () => {
    render(
      <SessionList
        sessions={[baseSession]}
        onSelect={() => {}}
        onRefresh={() => {}}
        onDelete={() => {}}
      />,
    );
    expect(screen.getByText("alpine:3.20")).toBeInTheDocument();
    expect(screen.getByText("aaaaaaaa")).toBeInTheDocument();
    expect(screen.getByText(/TTL/)).toBeInTheDocument();
  });

  it("fires the right callbacks on click", () => {
    const onSelect = vi.fn();
    const onRefresh = vi.fn();
    const onDelete = vi.fn();
    render(
      <SessionList
        sessions={[baseSession]}
        onSelect={onSelect}
        onRefresh={onRefresh}
        onDelete={onDelete}
      />,
    );
    fireEvent.click(screen.getByText("alpine:3.20"));
    expect(onSelect).toHaveBeenCalledWith(baseSession.id);
    fireEvent.click(screen.getByLabelText(/refresh/i));
    expect(onRefresh).toHaveBeenCalledWith(baseSession.id);
    fireEvent.click(screen.getByLabelText(/delete/i));
    expect(onDelete).toHaveBeenCalledWith(baseSession.id);
  });
});
