import assert from "node:assert/strict";
import test from "node:test";
import { QueryClient } from "@tanstack/react-query";
import { APIError, replaceSession, sessionQuery } from "../lib/api.ts";

test("a session poll that started during account rotation cannot publish its late 401", async () => {
  const client = new QueryClient();
  const previous = { username: "admin" };
  const updated = { username: "operator" };
  client.setQueryData(sessionQuery.queryKey, previous);

  const response = Promise.withResolvers();
  const started = Promise.withResolvers();
  try {
    // This poll starts after the mutation's initial cancellation.
    await client.cancelQueries({ queryKey: sessionQuery.queryKey });
    const poll = client.fetchQuery({
      ...sessionQuery,
      queryFn: () => {
        started.resolve();
        return response.promise;
      },
    }).catch(error => error);
    await started.promise;

    await replaceSession(client, updated);
    response.reject(new APIError(401, "Sign in to continue.", "UNAUTHORIZED"));
    await poll;

    const state = client.getQueryState(sessionQuery.queryKey);
    assert.deepEqual(state.data, updated);
    assert.equal(state.status, "success");
    assert.equal(state.error, null);
  } finally {
    client.clear();
  }
});
