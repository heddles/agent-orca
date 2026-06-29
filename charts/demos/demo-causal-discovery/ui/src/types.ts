export interface ExpertState {
  name: string
  active: boolean
  confidence: number
  evidenceCount: number
  lastUpdate: string
}

export interface EvidenceItem {
  id: string
  source: string
  title: string
  confidence: number
  timestamp: string
  content: string
}