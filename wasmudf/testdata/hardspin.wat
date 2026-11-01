;; hardspin -- an UNinstrumented runaway. Nothing in it ever yields to the host,
;; so no cooperative budget can see it and only the wall-clock backstop ends the
;; call. It exists to prove the backstop is real, and to keep the limits of the
;; step budget written down in a form that fails if they are ever overstated.
(module
  (memory (export "memory") 1)
  (func (export "classad_udf_abi_version") (result i32) (i32.const 1))
  (func (export "classad_udf_call") (param i32 i32 i32) (result i32)
    (loop $spin (br $spin))
    (i32.const 0))
)
