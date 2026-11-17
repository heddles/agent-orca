/**
 * Workspace (namespace) context. An empty string means "all workspaces", which
 * is what the API treats as no filter. The switcher in the top bar writes it;
 * list views read it instead of hard-coding a namespace.
 */
import { createContext, useContext, useMemo, useState, type ReactNode } from 'react'

interface NamespaceValue {
  namespace: string
  setNamespace: (ns: string) => void
}

const NamespaceContext = createContext<NamespaceValue>({ namespace: '', setNamespace: () => {} })

export function NamespaceProvider({ initial = '', children }: { initial?: string; children: ReactNode }) {
  const [namespace, setNamespace] = useState(initial)
  const value = useMemo(() => ({ namespace, setNamespace }), [namespace])
  return <NamespaceContext.Provider value={value}>{children}</NamespaceContext.Provider>
}

export function useNamespace(): NamespaceValue {
  return useContext(NamespaceContext)
}
